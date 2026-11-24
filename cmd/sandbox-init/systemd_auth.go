package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync/atomic"
)

// systemdAuthConn uses sd-bus's own EXTERNAL authentication ordering. Sending
// DATA and BEGIN together places BEGIN in the server's authentication buffer
// before it replies OK. The first binary call is sent only after that reply.
// With BEGIN sent after OK, supported sd-bus versions can read BEGIN and the
// call into their authentication buffer together, then leave the complete call
// unprocessed until another socket event. This is an ordering barrier, not a
// delay or a retry, and works with unmodified provided installations.
//
// godbus still validates the authentication exchange. Its auth bufio.Reader
// is not retained by the binary reader, so reads must not prefetch binary
// bytes after OK. The one-byte bound applies only during authentication.
// This private property/control connection never negotiates D-Bus Unix FDs;
// manager connection FDs travel on the separately authenticated executor link.
type systemdAuthConn struct {
	net.Conn
	phase atomic.Uint32 // 0: auth; 1: DATA+BEGIN sent; 2: binary
}

func (c *systemdAuthConn) Read(p []byte) (int, error) {
	if c.phase.Load() != 2 && len(p) > 1 {
		p = p[:1]
	}
	return c.Conn.Read(p)
}

func (c *systemdAuthConn) Write(p []byte) (int, error) {
	switch c.phase.Load() {
	case 2:
		return c.Conn.Write(p)
	case 1:
		if !bytes.Equal(p, []byte("BEGIN\r\n")) {
			return 0, errors.New("systemd EXTERNAL authentication did not complete after DATA")
		}
		// BEGIN was already sent with DATA. Auth starts godbus's binary reader
		// only after this write returns, so no auth reader remains at phase 2.
		c.phase.Store(2)
		return len(p), nil
	}
	if bytes.Equal(p, []byte("BEGIN\r\n")) {
		return 0, errors.New("systemd EXTERNAL authentication omitted its DATA barrier")
	}
	if !bytes.Equal(p, []byte("DATA\r\n")) {
		return c.Conn.Write(p)
	}
	data := []byte("DATA\r\nBEGIN\r\n")
	n, err := c.Conn.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return min(n, len(p)), err
	}
	c.phase.Store(1)
	return len(p), nil
}
