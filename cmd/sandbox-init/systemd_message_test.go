package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/godbus/dbus/v5"
)

type systemdReadError struct{ err error }

func (r systemdReadError) Read([]byte) (int, error) { return 0, r.err }

func TestSystemdPeerDecodeClosedConnectionAtMessagePadding(t *testing.T) {
	wire, err := systemdTestBusMessage(dbus.Message{
		Type: dbus.TypeSignal,
		Headers: map[dbus.HeaderField]dbus.Variant{
			dbus.FieldPath:      dbus.MakeVariant(dbus.ObjectPath("/org/freedesktop/systemd1")),
			dbus.FieldInterface: dbus.MakeVariant("org.freedesktop.systemd1.Manager"),
			dbus.FieldMember:    dbus.MakeVariant("JobRemoved"),
			dbus.FieldSignature: dbus.MakeVariant(dbus.SignatureOf(
				uint32(1), dbus.ObjectPath("/org/freedesktop/systemd1/job/1"), systemdRestoreTarget, "done")),
		},
		Body: []any{uint32(1), dbus.ObjectPath("/org/freedesktop/systemd1/job/1"), systemdRestoreTarget, "done"},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbus.DecodeMessage(bytes.NewReader(wire)); err != nil {
		t.Fatalf("complete JobRemoved message: %v", err)
	}
	headerEnd := 16 + int(binary.LittleEndian.Uint32(wire[12:16]))
	if headerEnd%8 == 0 || headerEnd >= len(wire) {
		t.Fatal("JobRemoved fixture must have padding before its body")
	}
	// Closing a private peer may interrupt its reader after the header and
	// before the body. The error must return to the connection worker.
	reader := io.MultiReader(bytes.NewReader(wire[:headerEnd]), systemdReadError{net.ErrClosed})
	if _, err := dbus.DecodeMessage(reader); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("decode closed peer: got %v, want %v", err, net.ErrClosed)
	}
}
