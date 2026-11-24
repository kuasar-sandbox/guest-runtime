package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/internal/wireio"
	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

// This protocol is private to two instances of the same executable. It is
// preserved together with them in a memory snapshot, not exposed as a Host ABI.
type executorMessage struct {
	ID      uint64            `json:"id,omitempty"`
	Reply   bool              `json:"reply,omitempty"`
	Op      string            `json:"op,omitempty"`
	Error   string            `json:"error,omitempty"`
	Launch  *proto.LaunchSpec `json:"launch,omitempty"`
	Request *proto.Message    `json:"request,omitempty"`
	PID     int               `json:"pid,omitempty"`
	Code    int               `json:"code,omitempty"`
	Signal  int               `json:"signal,omitempty"`
	Token   string            `json:"token,omitempty"`
	Epoch   uint64            `json:"epoch,omitempty"`
	Reason  string            `json:"reason,omitempty"`
}

type executorReply struct {
	message executorMessage
	err     error
}

type executorLink struct {
	conn      *net.UnixConn
	writing   chan struct{}
	mu        sync.Mutex
	next      uint64
	pending   map[uint64]chan executorReply
	done      chan struct{}
	closeOnce sync.Once
}

func newExecutorPair() (*executorLink, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	parent := os.NewFile(uintptr(fds[0]), "executor-parent")
	child := os.NewFile(uintptr(fds[1]), "executor-child")
	link, err := executorLinkFromFile(parent)
	_ = parent.Close()
	if err != nil {
		_ = child.Close()
		return nil, nil, err
	}
	return link, child, nil
}

func executorLinkFromFile(file *os.File) (*executorLink, error) {
	conn, err := net.FileConn(file)
	if err != nil {
		return nil, err
	}
	u, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("executor connection is not a Unix socket")
	}
	return newExecutorLink(u), nil
}

func newExecutorLink(conn *net.UnixConn) *executorLink {
	return &executorLink{conn: conn, writing: make(chan struct{}, 1), pending: make(map[uint64]chan executorReply), done: make(chan struct{})}
}

func (l *executorLink) close() {
	l.closeOnce.Do(func() {
		close(l.done)
		_ = l.conn.Close()
	})
}

// Rights accompany only the first header bytes. SOCK_STREAM allows launch and
// exec requests larger than a Unix datagram; the receiver never reads ahead into
// another frame. A failed write closes the channel rather than replaying an FD.
func (l *executorLink) send(msg executorMessage, fd int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return l.sendContext(ctx, msg, fd)
}

func (l *executorLink) sendContext(ctx context.Context, msg executorMessage, fd int) error {
	return l.sendFrame(ctx, msg, fd, false)
}

// A refresh has no execution side effect and uses its own discardable epoch.
// If no header byte or descriptor was sent, its timeout leaves the stream
// usable. Any partial frame is a real protocol failure and closes the link.
func (l *executorLink) sendControl(ctx context.Context, msg executorMessage, fd int) error {
	return l.sendFrame(ctx, msg, fd, true)
}

func (l *executorLink) sendFrame(ctx context.Context, msg executorMessage, fd int, allowUnsentTimeout bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if len(b) > proto.MaxMessageBytes+4096 {
		return errors.New("executor message too large")
	}
	select {
	case l.writing <- struct{}{}:
		defer func() { <-l.writing }()
	case <-ctx.Done():
		return ctx.Err()
	case <-l.done:
		return net.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	if requested, ok := ctx.Deadline(); ok && requested.Before(deadline) {
		deadline = requested
	}
	_ = l.conn.SetWriteDeadline(deadline)
	canceled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = l.conn.SetWriteDeadline(time.Now())
		close(canceled)
	})
	defer func() {
		if !stop() {
			<-canceled
		}
		_ = l.conn.SetWriteDeadline(time.Time{})
	}()
	header := make([]byte, 4)
	binary.LittleEndian.PutUint32(header, uint32(len(b)))
	var rights []byte
	if fd >= 0 {
		rights = unix.UnixRights(fd)
	}
	n, oobn, err := l.conn.WriteMsgUnix(header, rights, nil)
	if err != nil && allowUnsentTimeout && n == 0 && oobn == 0 {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return err
		}
	}
	if err == nil {
		err = wireio.WriteAll(l.conn, header[n:])
	}
	if err == nil {
		err = wireio.WriteAll(l.conn, b)
	}
	if err != nil {
		l.close()
	}
	return err
}

func (l *executorLink) receive() (_ executorMessage, fd int, err error) {
	fd = -1
	defer func() {
		if err != nil && fd >= 0 {
			_ = unix.Close(fd)
			fd = -1
		}
	}()
	header := make([]byte, 4)
	oob := make([]byte, unix.CmsgSpace(4))
	// Go's Linux ReadMsgUnix uses MSG_CMSG_CLOEXEC, including for the rights
	// received concurrently with an unrelated user-process fork.
	n, oobn, flags, _, err := l.conn.ReadMsgUnix(header, oob)
	if err != nil {
		return executorMessage{}, fd, err
	}
	control, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return executorMessage{}, fd, err
	}
	invalid := flags&unix.MSG_CTRUNC != 0
	for _, c := range control {
		rights, e := unix.ParseUnixRights(&c)
		if e != nil {
			invalid = true
			continue
		}
		for _, received := range rights {
			if fd < 0 {
				fd = received
			} else {
				_ = unix.Close(received)
				invalid = true
			}
		}
	}
	if invalid {
		return executorMessage{}, fd, errors.New("invalid executor descriptor message")
	}
	if n == 0 {
		return executorMessage{}, fd, io.EOF
	}
	if _, err = io.ReadFull(l.conn, header[n:]); err != nil {
		return executorMessage{}, fd, err
	}
	size := binary.LittleEndian.Uint32(header)
	if size == 0 || size > proto.MaxMessageBytes+4096 {
		return executorMessage{}, fd, errors.New("invalid executor message size")
	}
	b := make([]byte, size)
	if _, err = io.ReadFull(l.conn, b); err != nil {
		return executorMessage{}, fd, err
	}
	var msg executorMessage
	err = json.Unmarshal(b, &msg)
	return msg, fd, err
}

// A handler takes ownership of a descriptor by setting *fd to -1. Requests may
// call back over the same channel, so only the reader matches replies, and each
// handler runs separately. Lifecycle handlers provide their own serialization.
func (l *executorLink) serve(handler func(executorMessage, *int) (executorMessage, error)) {
	defer l.close()
	for {
		msg, fd, err := l.receive()
		if err != nil {
			return
		}
		if msg.Reply {
			if fd >= 0 {
				_ = unix.Close(fd)
			}
			l.mu.Lock()
			waiter := l.pending[msg.ID]
			l.mu.Unlock()
			if waiter != nil {
				var replyErr error
				if msg.Error != "" {
					replyErr = errors.New(msg.Error)
				}
				waiter <- executorReply{msg, replyErr}
			}
			continue
		}
		go func(msg executorMessage, fd int) {
			defer func() {
				if fd >= 0 {
					_ = unix.Close(fd)
				}
			}()
			resp, err := handler(msg, &fd)
			resp.ID, resp.Reply = msg.ID, true
			if err != nil {
				resp.Error = err.Error()
			}
			if msg.ID != 0 {
				_ = l.send(resp, -1)
			}
		}(msg, fd)
	}
}

func (l *executorLink) call(ctx context.Context, msg executorMessage, fd int) (executorMessage, error) {
	waiter := make(chan executorReply, 1)
	l.mu.Lock()
	l.next++
	msg.ID = l.next
	l.pending[msg.ID] = waiter
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.pending, msg.ID)
		l.mu.Unlock()
	}()
	if err := l.sendContext(ctx, msg, fd); err != nil {
		return executorMessage{}, err
	}
	select {
	case reply := <-waiter:
		return reply.message, reply.err
	case <-ctx.Done():
		// The operation may already own an FD or have mutated execution state.
		// Without a confirmed reply, continuing could let a late attach or
		// session cross a later quiesce. Fail the private link, never replay.
		l.close()
		return executorMessage{}, ctx.Err()
	case <-l.done:
		return executorMessage{}, errors.New("executor connection closed")
	}
}

func (l *executorLink) request(msg executorMessage, fd int) (executorMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return l.call(ctx, msg, fd)
}

// The primary is held at its pre-exec gate while the outer init reads this
// pidfd. fdinfo renders its PID in the reader's PID namespace; neither a path
// supplied by the child nor a namespace-relative PID is trusted as identity.
func outerPIDFromFD(fd int) (int, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", fd))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if value, ok := strings.CutPrefix(line, "Pid:"); ok {
			pid, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || pid <= 0 {
				return 0, errors.New("primary pidfd no longer identifies a process")
			}
			return pid, nil
		}
	}
	return 0, errors.New("primary identity descriptor is not a pidfd")
}
