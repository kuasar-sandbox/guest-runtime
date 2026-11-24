package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func systemdTestBusMessage(msg dbus.Message, serial uint32) ([]byte, error) {
	var wire bytes.Buffer
	if err := msg.EncodeTo(&wire, binary.LittleEndian); err != nil {
		return nil, err
	}
	binary.LittleEndian.PutUint32(wire.Bytes()[8:12], serial)
	return wire.Bytes(), nil
}

// Exercise the actual godbus Auth and first property call. The peer requires
// BEGIN before sending OK, then sends OK and its first binary signal together.
// This catches both the sd-bus authentication barrier and lost bufio read-ahead.
func TestSystemdAuthenticationPreservesFirstBinaryMessage(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	deadline := time.Now().Add(3 * time.Second)
	_ = client.SetDeadline(deadline)
	_ = server.SetDeadline(deadline)
	done := make(chan error, 1)
	go func() {
		done <- func() error {
			in := bufio.NewReader(server)
			first, err := in.ReadByte()
			if err != nil || first != 0 {
				return fmt.Errorf("initial credential byte: %d %v", first, err)
			}
			expect := func(want string) error {
				got, err := in.ReadString('\n')
				if err != nil || got != want {
					return fmt.Errorf("auth line: got %q, want %q: %v", got, want, err)
				}
				return nil
			}
			if err := expect("AUTH\r\n"); err != nil {
				return err
			}
			if _, err := io.WriteString(server, "REJECTED EXTERNAL\r\n"); err != nil {
				return err
			}
			if err := expect("AUTH EXTERNAL\r\n"); err != nil {
				return err
			}
			if _, err := io.WriteString(server, "DATA\r\n"); err != nil {
				return err
			}
			if err := expect("DATA\r\n"); err != nil {
				return err
			}
			if err := expect("BEGIN\r\n"); err != nil {
				return err
			}
			signal, err := systemdTestBusMessage(dbus.Message{
				Type: dbus.TypeSignal,
				Headers: map[dbus.HeaderField]dbus.Variant{
					dbus.FieldPath:      dbus.MakeVariant(dbus.ObjectPath("/org/freedesktop/systemd1")),
					dbus.FieldInterface: dbus.MakeVariant("org.freedesktop.systemd1.Manager"),
					dbus.FieldMember:    dbus.MakeVariant("StartupFinished"),
				},
			}, 1)
			if err != nil {
				return err
			}
			firstReply := append([]byte("OK 0123456789abcdef0123456789abcdef\r\n"), signal...)
			if _, err := server.Write(firstReply); err != nil {
				return err
			}
			call, err := dbus.DecodeMessage(in)
			if err != nil {
				return err
			}
			if call.Type != dbus.TypeMethodCall || call.Headers[dbus.FieldMember].Value() != "Get" {
				return fmt.Errorf("unexpected first binary call: %v", call)
			}
			reply, err := systemdTestBusMessage(dbus.Message{
				Type: dbus.TypeMethodReply,
				Headers: map[dbus.HeaderField]dbus.Variant{
					dbus.FieldReplySerial: dbus.MakeVariant(call.Serial()),
					dbus.FieldSignature:   dbus.MakeVariant(dbus.SignatureOf(dbus.MakeVariant("257.13"))),
				},
				Body: []any{dbus.MakeVariant("257.13")},
			}, 2)
			if err != nil {
				return err
			}
			_, err = server.Write(reply)
			return err
		}()
	}()
	stream := &systemdAuthConn{Conn: client}
	bus, err := dbus.NewConn(stream)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	signals := make(chan *dbus.Signal, 1)
	bus.Signal(signals)
	defer bus.RemoveSignal(signals)
	if err := bus.Auth([]dbus.Auth{dbus.AuthExternal("0")}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	var value dbus.Variant
	err = bus.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1").CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, "org.freedesktop.systemd1.Manager", "Version").Store(&value)
	if err != nil || value.Value() != "257.13" {
		t.Fatalf("first property call: %v %v", value, err)
	}
	select {
	case signal := <-signals:
		if signal.Name != "org.freedesktop.systemd1.Manager.StartupFinished" {
			t.Fatalf("first signal: %+v", signal)
		}
	case <-ctx.Done():
		t.Fatal("binary signal after OK was lost")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type systemdShortAuthConn struct{ net.Conn }

func (c systemdShortAuthConn) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestSystemdAuthenticationRejectsPartialOrUnconfirmedBarrier(t *testing.T) {
	stream := &systemdAuthConn{Conn: systemdShortAuthConn{}}
	if _, err := stream.Write([]byte("DATA\r\n")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("partial authentication reported success: %v", err)
	}
	if stream.phase.Load() != 0 {
		t.Fatal("partial write committed authentication")
	}
	if _, err := stream.Write([]byte("BEGIN\r\n")); err == nil {
		t.Fatal("accepted BEGIN without the confirmed DATA barrier")
	}
	stream.phase.Store(1)
	for _, invalid := range []string{"DATA\r\n", "CANCEL\r\n", "AUTH EXTERNAL\r\n"} {
		if _, err := stream.Write([]byte(invalid)); err == nil {
			t.Fatalf("accepted repeated auth %q", invalid)
		}
	}
}
