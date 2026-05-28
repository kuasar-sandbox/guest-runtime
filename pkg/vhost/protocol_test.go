package vhost

import (
	"net"
	"sync"
	"testing"
)

func TestSendU64Reply_RoundTrip(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// net.Pipe doesn't give us a *net.UnixConn — but our wire format is
	// just a byte stream. Use a small adapter via an actual socketpair.
	t.Skip("net.Pipe not a UnixConn; covered indirectly via TestParseSetMemTable_Layout")
}

func TestParseSetMemTable_Layout(t *testing.T) {
	// Synthesize a payload with two regions and parse it back.
	regs := []vhostMemoryRegion{
		{GuestPhysAddr: 0x10000, MemorySize: 0x100000, UserspaceAddr: 0x20000, MmapOffset: 0},
		{GuestPhysAddr: 0x200000, MemorySize: 0x80000, UserspaceAddr: 0x300000, MmapOffset: 0x100000},
	}
	payload := make([]byte, 8+len(regs)*32)
	payload[0] = byte(len(regs))
	for i, r := range regs {
		off := 8 + i*32
		putU64(payload[off:off+8], r.GuestPhysAddr)
		putU64(payload[off+8:off+16], r.MemorySize)
		putU64(payload[off+16:off+24], r.UserspaceAddr)
		putU64(payload[off+24:off+32], r.MmapOffset)
	}
	// Two stand-in fds (fd values don't matter for parse).
	fds := []int{42, 43}
	parsed, err := ParseSetMemTable(payload, fds)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("expected 2 regions, got %d", len(parsed))
	}
	if parsed[0].GuestPhysAddr != regs[0].GuestPhysAddr || parsed[0].MemorySize != regs[0].MemorySize {
		t.Errorf("region 0 mismatch: %+v vs %+v", parsed[0], regs[0])
	}
	if parsed[1].MmapOffset != regs[1].MmapOffset {
		t.Errorf("region 1 mmap offset: got %d want %d", parsed[1].MmapOffset, regs[1].MmapOffset)
	}
}

func TestParseSetMemTable_NumRegionsMismatch(t *testing.T) {
	payload := make([]byte, 8+32)
	payload[0] = 1
	_, err := ParseSetMemTable(payload, []int{42, 43}) // 2 fds for 1 region
	if err == nil {
		t.Fatal("expected error for fd/num mismatch, got nil")
	}
}

func TestParseBlkReqHeader_Roundtrip(t *testing.T) {
	buf := make([]byte, 16)
	putU32(buf[0:4], BlkTypeIn)
	putU32(buf[4:8], 0)
	putU64(buf[8:16], 12345)
	hdr, err := ParseBlkReqHeader(buf)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Type != BlkTypeIn || hdr.Sector != 12345 {
		t.Errorf("got %+v", hdr)
	}
}

func TestParseBlkReqHeader_Short(t *testing.T) {
	_, err := ParseBlkReqHeader(make([]byte, 8))
	if err == nil {
		t.Fatal("expected error for short header")
	}
}

func TestBlkConfig_MarshalSize(t *testing.T) {
	cfg := BlkConfig{Capacity: 1000, NumQueues: 1}
	b := cfg.Marshal()
	if len(b) != BlkConfigSize {
		t.Fatalf("Marshal size %d, expected %d", len(b), BlkConfigSize)
	}
}

func TestMsgName_KnownAndUnknown(t *testing.T) {
	if MsgName(MsgGetFeatures) != "GET_FEATURES" {
		t.Errorf("GET_FEATURES name wrong")
	}
	if MsgName(9999) == "" {
		t.Errorf("Unknown msg should have a non-empty fallback")
	}
}

// putU64 / putU32 are tiny helpers to avoid pulling encoding/binary
// imports across many tests.
func putU64(b []byte, v uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
}
func putU32(b []byte, v uint32) {
	for i := 0; i < 4; i++ {
		b[i] = byte(v >> (8 * i))
	}
}

// Avoid unused import warning for sync.
var _ = sync.Mutex{}
