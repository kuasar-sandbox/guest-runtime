package uffd

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// PageSize must match the granularity uffd was set up with.
const PageSize = 4096

// MaxBatchPages caps the number of contiguous pages a single fault
// event resolves in one UFFDIO_COPY / UFFDIO_ZEROPAGE call. 256 pages
// = 1 MiB. Hits two goals:
//  1. Amortize ioctl + per-fault wake overhead across many pages
//     (typical first-touch workloads page in adjacent VA ranges)
//  2. Keep the per-worker source buffer at 1 MiB (sync.Pool friendly,
//     no surprise multi-MiB allocations under fault burst)
const MaxBatchPages = 256

// MaxBatchBytes is MaxBatchPages × PageSize.
const MaxBatchBytes = MaxBatchPages * PageSize

// Config gathers everything the handler needs from the caller. Memfd /
// BackendVA / Size come from pkg/sandbox/memory.Memfd.
type Config struct {
	// Underlying memfd fd (for pread on the rare Loaded fault path).
	MemfdFD int

	// Sandbox-ctl-side mmap of the memfd. Used as the target for
	// reciprocal madvise(DONTNEED) on EVENT_REMOVE, and as the VMA
	// uffd_A registers MISSING on (so backend first-touch on
	// backendVA also routes through the handler — see §11).
	BackendVA uintptr
	Size      int

	// Source of truth for Absent-page contents. ZeroSource for cold
	// start; SparseSnapshotSource for restore.
	Source SnapshotReader

	// Number of worker goroutines. 0 → runtime.NumCPU(); minimum 2.
	NumWorkers int

	// Optional logger; nil → discarded.
	Logf func(string, ...any)
}

// Handler manages two userfaultfds:
//   - uffdA: created in this (sandbox-ctl) process, registered
//     MISSING on BackendVA. Catches first-touch from backend code
//     paths (e.g. vhost-user-blk reading/writing guest buffers).
//   - uffdC: created in CH's process, registered MISSING on chVA,
//     handed to us via SCM_RIGHTS in the va_report handshake.
//     Catches first-touch from vCPU.
//
// Both fd's events feed the same fault-resolution logic. EEXIST race
// recovery via UFFDIO_WAKE (then kernel auto-installs PTE on retry,
// since MINOR is not registered).
//
// 5.10+ kernel compatible — no MINOR_SHMEM / UFFDIO_CONTINUE required.
type Handler struct {
	cfg Config

	uffdC       *os.File // CH-mm uffd, owns the inherited fd
	addrMap     *AddressMap
	state       *PageStateMap
	overrideMap *OverrideMap // backend-supplied bytes pending fault-time install

	epfd int

	logf  func(string, ...any)
	stop  chan struct{}
	wg    sync.WaitGroup
	queue []chan faultEvent

	stats handlerStats
}

type handlerStats struct {
	faultsAbsent   atomic.Uint64
	faultsReleased atomic.Uint64
	zeropages      atomic.Uint64 // count of UFFDIO_ZEROPAGE calls
	copies         atomic.Uint64 // count of UFFDIO_COPY calls
	pagesZeroed    atomic.Uint64 // total pages installed via ZEROPAGE
	pagesCopied    atomic.Uint64 // total pages installed via COPY
	wakes          atomic.Uint64
	removeEvents   atomic.Uint64
	errors         atomic.Uint64
	batchPagesSum  atomic.Uint64 // sum of run lengths (for avg calc)
	batchCalls     atomic.Uint64 // count of batches (avg = sum/calls)
	batchMaxPages  atomic.Uint64 // largest single batch observed
}

type faultEvent struct {
	address uint64
	flags   uint64
	uffdFD  int // which uffd this came from — determines ioctl target fd
}

// NewWithBackendUffd constructs the dual-uffd handler. The CH-side
// uffd (uffdCFromCH) is provided by the va_report OnReady callback;
// this constructor creates uffdA in the current process.
//
// Steps:
//  1. userfaultfd() in sandbox-ctl mm
//  2. UFFDIO_API with MISSING_SHMEM | EVENT_REMOVE | EVENT_UNMAP | THREAD_ID
//  3. UFFDIO_REGISTER MISSING on [BackendVA, +Size)
//  4. epoll_create1 → add uffdA + uffdC
//  5. AddressMap registers ProcessBackend (this constructor) and
//     ProcessCH (caller did before calling us)
//  6. Page state initialized to all-Absent
//
// Start the goroutines via Start(); stop via Close().
//
// Single-uffd architecture: only uffdC (CH-side) is registered. The
// backendVA mmap that sandbox-ctl holds is left WITHOUT a userfaultfd —
// kernel handles backend-mm faults directly via shmem fileops. Since
// vhost backends never directly mutate a folio that the handler hasn't
// already created (they go through Handler.WritePage which routes
// through pwrite for Loaded pages and OverrideMap for Absent pages),
// the cross-mm folio-creation race that produced the WSL2 wake loop
// in dual-uffd mode cannot trigger.
func NewWithBackendUffd(uffdCFromCH int, addrMap *AddressMap, cfg Config) (*Handler, error) {
	if uffdCFromCH <= 0 {
		return nil, fmt.Errorf("uffd: uffdCFromCH invalid")
	}
	if cfg.MemfdFD <= 0 {
		return nil, fmt.Errorf("uffd: MemfdFD invalid")
	}
	if cfg.Size <= 0 || cfg.Size%PageSize != 0 {
		return nil, fmt.Errorf("uffd: Size %d not page-aligned", cfg.Size)
	}
	if cfg.Source == nil {
		return nil, fmt.Errorf("uffd: Source is nil")
	}
	if addrMap == nil {
		return nil, fmt.Errorf("uffd: AddressMap is nil")
	}
	if cfg.NumWorkers <= 0 {
		cfg.NumWorkers = runtime.NumCPU()
	}
	if cfg.NumWorkers < 2 {
		cfg.NumWorkers = 2
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	// epoll for the single uffd (uffdC, owned by CH, sent over SCM_RIGHTS).
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("epoll_create1: %w", err)
	}
	ev := unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(uffdCFromCH)}
	if err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, uffdCFromCH, &ev); err != nil {
		_ = unix.Close(epfd)
		return nil, fmt.Errorf("epoll_ctl ADD uffdC fd=%d: %w", uffdCFromCH, err)
	}

	numPages := cfg.Size / PageSize
	state := NewPageStateMap(numPages)

	queues := make([]chan faultEvent, cfg.NumWorkers)
	for i := range queues {
		queues[i] = make(chan faultEvent, 256)
	}

	return &Handler{
		cfg:         cfg,
		uffdC:       os.NewFile(uintptr(uffdCFromCH), "uffd_C_chVA"),
		addrMap:     addrMap,
		state:       state,
		overrideMap: NewOverrideMap(),
		epfd:        epfd,
		logf:        logf,
		stop:        make(chan struct{}),
		queue:       queues,
	}, nil
}

// AddressMap exposes the map for the va_report server to register
// ProcessCH on handshake (must happen BEFORE the first uffdC fault,
// so the handler can translate ev.address → memfd offset).
func (h *Handler) AddressMap() *AddressMap { return h.addrMap }

// WritePage routes one page worth of backend-supplied data into the
// memfd. It is the only sanctioned way for vhost backends to update
// guest memory in the single-uffd architecture; bypassing this and
// pwrite-ing memfd directly creates folios outside the handler and
// re-introduces the EEXIST / wake-loop race on uffdC.
//
// memfdOffset must be page-aligned. data length must be ≤ PageSize;
// callers split larger writes across pages.
//
//	state == Loaded:
//	    Handler has already created folio + PTE for chVA[N] via
//	    UFFDIO_COPY/ZEROPAGE. CH's PTE points to that folio. pwrite
//	    on memfd updates folio bytes; CH sees the new bytes on next
//	    read with no further fault.
//
//	state ∈ {Absent, Released}:
//	    Folio does not yet exist. Buffer the data in OverrideMap;
//	    handler.handleFault consumes it on the next uffdC fault for
//	    this page and resolves via UFFDIO_COPY (atomic folio + PTE).
//
// Safe for concurrent calls from many vhost workers.
func (h *Handler) WritePage(memfdOffset uint64, data []byte) error {
	if memfdOffset%PageSize != 0 {
		return fmt.Errorf("uffd: WritePage offset 0x%x not page-aligned", memfdOffset)
	}
	if uint64(len(data)) > PageSize {
		return fmt.Errorf("uffd: WritePage data %d > PageSize", len(data))
	}
	pageIdx := memfdOffset / PageSize
	if pageIdx >= uint64(h.state.Len()) {
		return fmt.Errorf("uffd: WritePage offset 0x%x past memfd size", memfdOffset)
	}
	if h.state.Get(pageIdx) == StateLoaded {
		// Folio + PTE for chVA already installed by handler. pwrite
		// the bytes; kernel updates the folio in place.
		_, err := unix.Pwrite(h.cfg.MemfdFD, data, int64(memfdOffset))
		if err != nil {
			return fmt.Errorf("uffd: pwrite memfd offset 0x%x: %w", memfdOffset, err)
		}
		return nil
	}
	// Absent / Released: queue for handler.handleFault to install
	// when CH next touches this page.
	buf := make([]byte, PageSize)
	copy(buf, data)
	h.overrideMap.Set(pageIdx, buf)
	return nil
}

// Start spawns the reader and worker goroutines.
func (h *Handler) Start() {
	h.wg.Add(1)
	go h.runReader()
	for i := range h.queue {
		h.wg.Add(1)
		go h.runWorker(i)
	}
}

// Close stops the reader+workers and closes uffdC + epfd.
func (h *Handler) Close() error {
	select {
	case <-h.stop:
		return nil
	default:
		close(h.stop)
	}
	// Closing the uffd fd breaks epoll_wait blocked in reader.
	if h.uffdC != nil {
		_ = h.uffdC.Close()
	}
	if h.epfd >= 0 {
		_ = unix.Close(h.epfd)
		h.epfd = -1
	}
	h.wg.Wait()
	for _, q := range h.queue {
		close(q)
	}
	return nil
}

// runReader pulls events from both uffds via epoll. Per-uffd reads
// are non-blocking (O_NONBLOCK on each fd); we drain whichever fd
// epoll reports ready, until EAGAIN, then wait again.
func (h *Handler) runReader() {
	defer h.wg.Done()
	const msgSize = int(unsafe.Sizeof(uffdMsg{}))
	buf := make([]byte, msgSize*16)
	events := make([]unix.EpollEvent, 4)
	for {
		select {
		case <-h.stop:
			return
		default:
		}
		n, err := unix.EpollWait(h.epfd, events, 200)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if errors.Is(err, unix.EBADF) {
				return
			}
			h.logf("uffd: epoll_wait: %v", err)
			h.stats.errors.Add(1)
			return
		}
		if n == 0 {
			continue
		}
		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)
			h.drain(fd, buf, msgSize)
		}
	}
}

func (h *Handler) drain(fd int, buf []byte, msgSize int) {
	for {
		n, err := unix.Read(fd, buf)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) {
				return
			}
			if errors.Is(err, unix.EBADF) {
				return
			}
			h.logf("uffd: read fd=%d: %v", fd, err)
			h.stats.errors.Add(1)
			return
		}
		if n == 0 {
			return
		}
		if n%msgSize != 0 {
			h.logf("uffd: short read fd=%d %d not multiple of %d", fd, n, msgSize)
			h.stats.errors.Add(1)
			continue
		}
		for off := 0; off < n; off += msgSize {
			msg := (*uffdMsg)(unsafe.Pointer(&buf[off]))
			h.dispatch(msg, fd)
		}
	}
}

func (h *Handler) dispatch(msg *uffdMsg, fromFD int) {
	switch msg.Event {
	case uffdEventPagefault:
		pf := (*uffdMsgPagefault)(unsafe.Pointer(&msg.Arg[0]))
		offset, ok := h.addrMap.Locate(pf.Address)
		if !ok {
			h.logf("uffd: fault at unknown va 0x%x (fd=%d)", pf.Address, fromFD)
			h.stats.errors.Add(1)
			_ = ioctlUffdWake(fromFD, pf.Address&^(PageSize-1), PageSize)
			return
		}
		idx := offset / PageSize
		hashed := pageIdxHash(idx) % uint64(len(h.queue))
		select {
		case h.queue[hashed] <- faultEvent{address: pf.Address, flags: pf.Flags, uffdFD: fromFD}:
		case <-h.stop:
		}
	case uffdEventRemove, uffdEventUnmap:
		rm := (*uffdMsgRemove)(unsafe.Pointer(&msg.Arg[0]))
		h.handleRemove(rm.Start, rm.End, fromFD)
		h.stats.removeEvents.Add(1)
	default:
		h.logf("uffd: unexpected event 0x%x on fd=%d", msg.Event, fromFD)
	}
}

func (h *Handler) handleRemove(startVA, endVA uint64, fromFD int) {
	startOff, ok := h.addrMap.Locate(startVA)
	if !ok {
		h.logf("uffd: EVENT_REMOVE start 0x%x unknown", startVA)
		return
	}
	endOff := startOff + (endVA - startVA)
	startPage := startOff / PageSize
	endPage := (endOff + PageSize - 1) / PageSize
	h.state.SetRange(startPage, endPage, StateReleased)

	// Reciprocal madvise(DONTNEED) on the OTHER side's VMA so the
	// shmem inode page reference drops on both sides → kernel really
	// frees the physical page. Pick the opposite VMA from where the
	// EVENT_REMOVE came.
	var otherProcess ProcessKind
	if fromFD == int(h.uffdC.Fd()) {
		otherProcess = ProcessBackend
	} else {
		otherProcess = ProcessCH
	}
	otherVMAStart, ok := h.addrMap.VMAStart(otherProcess)
	if !ok {
		return
	}
	if otherProcess == ProcessBackend {
		// We can madvise our own backend mm directly.
		addr := otherVMAStart + startOff
		length := uintptr(endPage-startPage) * PageSize
		if length == 0 {
			return
		}
		_, _, errno := unix.Syscall(unix.SYS_MADVISE,
			uintptr(addr), length, uintptr(unix.MADV_DONTNEED))
		if errno != 0 {
			h.logf("uffd: madvise DONTNEED backendVA 0x%x len=%d failed: %v",
				addr, length, errno)
			h.stats.errors.Add(1)
		}
	}
	// If the EVENT_REMOVE came from uffdA (sandbox-ctl side), the
	// reciprocal would be madvise on chVA in CH's mm — we can't do
	// that from here. CH itself doesn't issue DONTNEED on its own
	// VMA when sandbox-ctl side does, so this case is mostly dormant
	// (sandbox-ctl never madvises backendVA on its own initiative).
	// Left as a note; not exercised in P1.5/P2/P3 paths.
}

func (h *Handler) runWorker(idx int) {
	defer h.wg.Done()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// Per-worker buffer sized for the maximum batch (1 MiB). Sticking
	// to a fixed buffer avoids per-fault allocation in the hot path.
	pageBuf := make([]byte, MaxBatchBytes)
	q := h.queue[idx]
	for {
		select {
		case <-h.stop:
			return
		case ev, ok := <-q:
			if !ok {
				return
			}
			h.handleFault(ev, pageBuf)
		}
	}
}

// absentRunFrom returns the number of contiguous StateAbsent pages
// starting at pageIdx, capped at MaxBatchPages and at the end of the
// page-state table. State-only walk — no Source involvement; the
// Source then caps further within this bound based on its internal
// boundaries.
func (h *Handler) absentRunFrom(pageIdx uint64) uint64 {
	stateLen := uint64(h.state.Len())
	n := uint64(1)
	for n < MaxBatchPages && pageIdx+n < stateLen {
		if h.state.Get(pageIdx+n) != StateAbsent {
			break
		}
		n++
	}
	return n
}

// extendReleasedBatch returns the longest run of Released pages at
// pageIdx (capped at MaxBatchPages). Released runs all resolve via
// UFFDIO_ZEROPAGE; classification is purely state-based.
func (h *Handler) extendReleasedBatch(pageIdx uint64) uint64 {
	stateLen := uint64(h.state.Len())
	n := uint64(1)
	for n < MaxBatchPages && pageIdx+n < stateLen {
		if h.state.Get(pageIdx+n) != StateReleased {
			break
		}
		n++
	}
	return n
}

func (h *Handler) recordBatch(pages uint64) {
	h.stats.batchCalls.Add(1)
	h.stats.batchPagesSum.Add(pages)
	for {
		cur := h.stats.batchMaxPages.Load()
		if pages <= cur || h.stats.batchMaxPages.CompareAndSwap(cur, pages) {
			break
		}
	}
}

func (h *Handler) handleFault(ev faultEvent, pageBuf []byte) {
	memfdOffset, ok := h.addrMap.Locate(ev.address)
	if !ok {
		h.logf("uffd: handleFault unknown va 0x%x", ev.address)
		h.stats.errors.Add(1)
		_ = ioctlUffdWake(ev.uffdFD, ev.address&^(PageSize-1), PageSize)
		return
	}
	pageVA := ev.address &^ (PageSize - 1)
	pageOffset := memfdOffset &^ (PageSize - 1)
	pageIdx := pageOffset / PageSize

	state := h.state.Get(pageIdx)
	switch state {
	case StateAbsent:
		h.stats.faultsAbsent.Add(1)

		// Backend may have queued a per-page override (vhost-blk
		// read response, snapshot data) for this page. Override
		// takes precedence over the source — handle one page at a
		// time and leave run-batching to subsequent faults.
		if data, ok := h.overrideMap.Pop(pageIdx); ok {
			copy(pageBuf[:PageSize], data)
			src := uint64(uintptr(unsafe.Pointer(&pageBuf[0])))
			err := ioctlUffdCopy(ev.uffdFD, pageVA, src, PageSize)
			if err != nil && !errors.Is(err, unix.EEXIST) {
				h.logf("uffd: COPY(override) va=0x%x: %v", pageVA, err)
				h.stats.errors.Add(1)
				_ = ioctlUffdWake(ev.uffdFD, pageVA, PageSize)
				return
			}
			if errors.Is(err, unix.EEXIST) {
				// Folio appeared concurrently — wake the thread.
				_ = ioctlUffdWake(ev.uffdFD, pageVA, PageSize)
				h.stats.wakes.Add(1)
			}
			h.stats.copies.Add(1)
			h.stats.pagesCopied.Add(1)
			h.state.Set(pageIdx, StateLoaded)
			h.recordBatch(1)
			return
		}

		// State-side cap: how many consecutive Absent pages from
		// pageIdx. Source then picks any n ≤ this, aligned to its
		// own internal boundaries (chunk for manifest, IsZero
		// transition for sparse).
		capPages := h.absentRunFrom(pageIdx)
		capBytes := capPages * PageSize

		n, isZero, err := h.cfg.Source.ReadAt(pageBuf[:capBytes], pageOffset)
		if err != nil && !errors.Is(err, io.EOF) {
			h.logf("uffd: source.ReadAt off=0x%x cap=%d: %v", pageOffset, capBytes, err)
			h.stats.errors.Add(1)
			_ = ioctlUffdWake(ev.uffdFD, pageVA, PageSize)
			return
		}
		runBytes := uint64(n)
		// Source returns 0 only at EOF — fall back to one zero page so
		// the faulting thread makes progress instead of looping.
		if runBytes == 0 {
			runBytes = PageSize
			isZero = true
		}
		if runBytes%PageSize != 0 {
			// Source returned a partial last page near EOF. Round up
			// and zero-pad; UFFDIO_* ioctls require page-aligned len.
			pad := PageSize - runBytes%PageSize
			for i := uint64(n); i < runBytes+pad; i++ {
				pageBuf[i] = 0
			}
			runBytes += pad
		}
		runPages := runBytes / PageSize

		if isZero {
			err := ioctlUffdZeropage(ev.uffdFD, pageVA, runBytes)
			if err != nil {
				if errors.Is(err, unix.EEXIST) {
					// Some page in the run already has a folio
					// (other uffd raced). Wake the faulted thread;
					// kernel auto-installs PTE on retry (MISSING-only,
					// folio-found path bypasses uffd).
					_ = ioctlUffdWake(ev.uffdFD, pageVA, runBytes)
					h.stats.wakes.Add(1)
				} else {
					h.logf("uffd: ZEROPAGE va=0x%x len=%d: %v", pageVA, runBytes, err)
					h.stats.errors.Add(1)
					return
				}
			}
			h.stats.zeropages.Add(1)
			h.stats.pagesZeroed.Add(runPages)
		} else {
			src := uint64(uintptr(unsafe.Pointer(&pageBuf[0])))
			if err := ioctlUffdCopy(ev.uffdFD, pageVA, src, runBytes); err != nil {
				if errors.Is(err, unix.EEXIST) {
					_ = ioctlUffdWake(ev.uffdFD, pageVA, runBytes)
					h.stats.wakes.Add(1)
				} else {
					h.logf("uffd: COPY va=0x%x len=%d: %v", pageVA, runBytes, err)
					h.stats.errors.Add(1)
					return
				}
			}
			h.stats.copies.Add(1)
			h.stats.pagesCopied.Add(runPages)
		}
		// Mark the run Loaded so subsequent faults short-circuit.
		for i := uint64(0); i < runPages; i++ {
			h.state.Set(pageIdx+i, StateLoaded)
		}
		h.recordBatch(runPages)

	case StateReleased:
		h.stats.faultsReleased.Add(1)
		runPages := h.extendReleasedBatch(pageIdx)
		runBytes := runPages * PageSize
		if err := ioctlUffdZeropage(ev.uffdFD, pageVA, runBytes); err != nil {
			if errors.Is(err, unix.EEXIST) {
				_ = ioctlUffdWake(ev.uffdFD, pageVA, runBytes)
				h.stats.wakes.Add(1)
			} else {
				h.logf("uffd: ZEROPAGE(released) va=0x%x len=%d: %v",
					pageVA, runBytes, err)
				h.stats.errors.Add(1)
				return
			}
		}
		h.stats.zeropages.Add(1)
		h.stats.pagesZeroed.Add(runPages)
		for i := uint64(0); i < runPages; i++ {
			h.state.Set(pageIdx+i, StateLoaded)
		}
		h.recordBatch(runPages)

	case StateLoaded:
		// MISSING-only registration: kernel installs PTE automatically
		// when folio exists. A userfault arriving here is a transient
		// race; just wake — kernel re-fault path auto-installs.
		_ = ioctlUffdWake(ev.uffdFD, pageVA, PageSize)
		h.stats.wakes.Add(1)
	}
}

func pageIdxHash(idx uint64) uint64 {
	h := fnv.New64a()
	var b [8]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(idx >> (i * 8))
	}
	_, _ = h.Write(b[:])
	return h.Sum64()
}

// Stats returns a snapshot of counter values for diagnostics.
func (h *Handler) Stats() map[string]uint64 {
	calls := h.stats.batchCalls.Load()
	pagesSum := h.stats.batchPagesSum.Load()
	avgBatch := uint64(0)
	if calls > 0 {
		avgBatch = pagesSum / calls
	}
	return map[string]uint64{
		"faults_absent":     h.stats.faultsAbsent.Load(),
		"faults_released":   h.stats.faultsReleased.Load(),
		"zeropage_calls":    h.stats.zeropages.Load(),
		"copy_calls":        h.stats.copies.Load(),
		"pages_zeroed":      h.stats.pagesZeroed.Load(),
		"pages_copied":      h.stats.pagesCopied.Load(),
		"wakes":             h.stats.wakes.Load(),
		"remove_events":     h.stats.removeEvents.Load(),
		"errors":            h.stats.errors.Load(),
		"batch_calls":       calls,
		"batch_pages_total": pagesSum,
		"batch_avg_pages":   avgBatch,
		"batch_max_pages":   h.stats.batchMaxPages.Load(),
	}
}
