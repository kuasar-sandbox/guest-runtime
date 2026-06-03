package fwd

import (
	"bytes"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	cases := []Frame{
		{Type: FrameData, Payload: []byte("hello world")},
		{Type: FrameData, Payload: bytes.Repeat([]byte{0xab}, MaxFramePayload)},
		{Type: FrameEOF},
		{Type: FrameRST},
		{Type: FrameData, Payload: nil}, // empty data frame
	}
	var buf bytes.Buffer
	for _, in := range cases {
		if err := WriteFrame(&buf, in); err != nil {
			t.Fatalf("WriteFrame(%d): %v", in.Type, err)
		}
	}
	for i, want := range cases {
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame #%d: %v", i, err)
		}
		if got.Type != want.Type {
			t.Errorf("#%d type: got %d want %d", i, got.Type, want.Type)
		}
		if !bytes.Equal(got.Payload, want.Payload) {
			t.Errorf("#%d payload: got %d bytes want %d bytes", i, len(got.Payload), len(want.Payload))
		}
	}
	if _, err := ReadFrame(&buf); err != io.EOF {
		t.Errorf("trailing ReadFrame: got %v want EOF", err)
	}
}

func TestWriteFrameTooLarge(t *testing.T) {
	var buf bytes.Buffer
	err := WriteFrame(&buf, Frame{Type: FrameData, Payload: make([]byte, MaxFramePayload+1)})
	if err != ErrFrameTooLarge {
		t.Fatalf("got %v want ErrFrameTooLarge", err)
	}
}

// shortReader returns one byte per Read so ReadFrame must reassemble a
// header/payload split across reads (io.ReadFull semantics).
type shortReader struct{ r io.Reader }

func (s shortReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return s.r.Read(p)
}

func TestReadFrameReassembles(t *testing.T) {
	var buf bytes.Buffer
	want := Frame{Type: FrameData, Payload: []byte("split across reads")}
	if err := WriteFrame(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(shortReader{r: &buf})
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if got.Type != want.Type || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
