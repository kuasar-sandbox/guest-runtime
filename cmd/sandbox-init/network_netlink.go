package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

type networkReply struct {
	Type  uint16
	Flags uint16
	Data  []byte
}

type networkRequester interface {
	request(context.Context, uint16, uint16, []byte) ([]networkReply, error)
}

// A connection is owned by one network operation, used sequentially and closed
// before the operation returns. Callers supply the operation's bounded context.
type networkConn struct {
	fd     int
	port   uint32
	seq    uint32
	broken bool
	buf    []byte
}

const networkReplyLimit = 8 << 20

func openNetworkConn(protocol int) (*networkConn, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, protocol)
	if err != nil {
		return nil, fmt.Errorf("open netlink: %w", err)
	}
	c := &networkConn{fd: fd, buf: make([]byte, 64<<10)}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		c.close()
		return nil, fmt.Errorf("bind netlink: %w", err)
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		c.close()
		return nil, fmt.Errorf("netlink port: %w", err)
	}
	nl, ok := sa.(*unix.SockaddrNetlink)
	if !ok || nl.Pid == 0 {
		c.close()
		return nil, fmt.Errorf("missing netlink local port")
	}
	c.port = nl.Pid
	return c, nil
}

func (c *networkConn) close() {
	if c.fd >= 0 {
		_ = unix.Close(c.fd)
		c.fd = -1
	}
}

// request never returns a partial dump as success. A timeout, malformed reply or
// interrupted dump retires the connection: an unread reply must not be consumed
// as the response to a later request. A complete kernel errno ACK is safe to
// follow with another request (in particular an exact socket can vanish).
func (c *networkConn) request(ctx context.Context, kind, flags uint16, payload []byte) (_ []networkReply, err error) {
	if c.fd < 0 {
		return nil, net.ErrClosed
	}
	if c.broken {
		return nil, fmt.Errorf("netlink connection retired after incomplete request")
	}
	if err := networkContextErr(ctx); err != nil {
		return nil, err
	}
	defer func() {
		var terminal *networkKernelError
		if err != nil && !errors.As(err, &terminal) {
			c.broken = true
		}
	}()
	c.seq++
	if c.seq == 0 {
		c.seq++
	}
	buf := make([]byte, unix.SizeofNlMsghdr+len(payload))
	binary.NativeEndian.PutUint32(buf, uint32(len(buf)))
	binary.NativeEndian.PutUint16(buf[4:], kind)
	binary.NativeEndian.PutUint16(buf[6:], flags|unix.NLM_F_REQUEST)
	binary.NativeEndian.PutUint32(buf[8:], c.seq)
	binary.NativeEndian.PutUint32(buf[12:], c.port)
	copy(buf[unix.SizeofNlMsghdr:], payload)
	for {
		if err := networkContextErr(ctx); err != nil {
			return nil, err
		}
		err := unix.Sendto(c.fd, buf, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK})
		if err == nil {
			break
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if !errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("send netlink: %w", err)
		}
		if err := waitNetworkFD(ctx, c.fd, unix.POLLOUT); err != nil {
			return nil, err
		}
	}
	s := networkResponse{
		seq: c.seq, port: c.port, requestType: kind,
		requestLen: uint32(len(buf)), requestFlags: flags | unix.NLM_F_REQUEST,
		dump:    flags&unix.NLM_F_DUMP == unix.NLM_F_DUMP,
		wantACK: flags&unix.NLM_F_ACK != 0,
	}
	for {
		if err := networkContextErr(ctx); err != nil {
			return nil, err
		}
		n, _, recvFlags, sender, err := unix.Recvmsg(c.fd, c.buf, nil, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EAGAIN) {
			if err := waitNetworkFD(ctx, c.fd, unix.POLLIN); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("receive netlink: %w", err)
		}
		nl, ok := sender.(*unix.SockaddrNetlink)
		if !ok || nl.Pid != 0 || nl.Groups != 0 {
			return nil, fmt.Errorf("netlink reply not from unicast kernel sender")
		}
		done, err := s.consume(c.buf[:n], recvFlags)
		if err != nil {
			return nil, err
		}
		if done {
			if err := networkContextErr(ctx); err != nil {
				return nil, err
			}
			return s.replies, nil
		}
	}
}

// Poll in short slices so a lifecycle cancellation does not wait for the
// original full timeout. Readiness never extends the caller's deadline.
func waitNetworkFD(ctx context.Context, fd int, events int16) error {
	for {
		if err := networkContextErr(ctx); err != nil {
			return err
		}
		wait := 10 * time.Millisecond
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return context.DeadlineExceeded
			}
			if remaining < wait {
				wait = remaining
			}
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: events}}
		// poll has millisecond precision; round up rather than busy-spinning
		// for the final fractional millisecond.
		ms := int((wait + time.Millisecond - 1) / time.Millisecond)
		n, err := unix.Poll(fds, ms)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("poll netlink: %w", err)
		}
		if err := networkContextErr(ctx); err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if fds[0].Revents&unix.POLLNVAL != 0 {
			return fmt.Errorf("poll netlink: %w", unix.EBADF)
		}
		if fds[0].Revents&events != 0 {
			return nil
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLHUP) != 0 {
			return fmt.Errorf("netlink poll error events %#x", fds[0].Revents)
		}
	}
}

// Check the absolute deadline as well as Err: a ready fd must not make progress
// past the budget while the context timer's goroutine is waiting to run.
func networkContextErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

type networkKernelError struct{ errno unix.Errno }

func (e *networkKernelError) Error() string { return "netlink kernel error: " + e.errno.Error() }
func (e *networkKernelError) Unwrap() error { return e.errno }

type networkResponse struct {
	seq, port    uint32
	requestType  uint16
	requestLen   uint32
	requestFlags uint16
	dump         bool
	wantACK      bool
	multipart    bool
	ack          bool
	done         bool
	bytes        int
	replies      []networkReply
}

func (s *networkResponse) consume(data []byte, recvFlags int) (bool, error) {
	if recvFlags&unix.MSG_TRUNC != 0 {
		return false, fmt.Errorf("truncated netlink datagram")
	}
	if len(data) == 0 {
		return false, fmt.Errorf("empty netlink datagram")
	}
	var terminalErr error
	for len(data) != 0 {
		if len(data) < unix.SizeofNlMsghdr {
			return false, fmt.Errorf("short netlink header")
		}
		length := int(binary.NativeEndian.Uint32(data))
		if length < unix.SizeofNlMsghdr || length > len(data) {
			return false, fmt.Errorf("invalid netlink message length %d", length)
		}
		kind := binary.NativeEndian.Uint16(data[4:])
		flags := binary.NativeEndian.Uint16(data[6:])
		seq := binary.NativeEndian.Uint32(data[8:])
		port := binary.NativeEndian.Uint32(data[12:])
		// Unicast kernel responses may use either 0 or the destination port
		// in nlmsg_pid; the recvmsg sockaddr must always identify the kernel.
		if seq != s.seq || (port != 0 && port != s.port) {
			return false, fmt.Errorf("netlink response sequence/port mismatch")
		}
		if flags&unix.NLM_F_DUMP_INTR != 0 {
			return false, fmt.Errorf("interrupted netlink dump")
		}
		if s.done || terminalErr != nil {
			return false, fmt.Errorf("netlink data after terminal reply")
		}
		if flags&unix.NLM_F_MULTI != 0 {
			s.multipart = true
		}
		body := data[unix.SizeofNlMsghdr:length]
		switch kind {
		case unix.NLMSG_NOOP:
		case unix.NLMSG_OVERRUN:
			return false, fmt.Errorf("netlink overrun: %w", unix.ENOBUFS)
		case unix.NLMSG_ERROR:
			if len(body) < 4+unix.SizeofNlMsghdr {
				return false, fmt.Errorf("short netlink ACK")
			}
			original := body[4:]
			if binary.NativeEndian.Uint32(original) != s.requestLen ||
				binary.NativeEndian.Uint16(original[4:]) != s.requestType ||
				binary.NativeEndian.Uint16(original[6:]) != s.requestFlags ||
				binary.NativeEndian.Uint32(original[8:]) != s.seq ||
				binary.NativeEndian.Uint32(original[12:]) != s.port {
				return false, fmt.Errorf("netlink ACK request mismatch")
			}
			if err := networkErrno(body); err != nil {
				if _, ok := err.(*networkKernelError); !ok {
					return false, err
				}
				terminalErr = err
			}
			s.ack = true
		case unix.NLMSG_DONE:
			if !s.dump && !s.multipart {
				return false, fmt.Errorf("unexpected netlink DONE")
			}
			if len(body) < 4 {
				return false, fmt.Errorf("short netlink DONE status")
			}
			if err := networkErrno(body); err != nil {
				if _, ok := err.(*networkKernelError); !ok {
					return false, err
				}
				terminalErr = err
			}
			s.done = true
		default:
			if kind < unix.NLMSG_MIN_TYPE {
				return false, fmt.Errorf("unexpected netlink control message %d", kind)
			}
			s.bytes += length
			if s.bytes > networkReplyLimit {
				return false, fmt.Errorf("netlink response exceeds %d bytes", networkReplyLimit)
			}
			s.replies = append(s.replies, networkReply{Type: kind, Flags: flags, Data: append([]byte(nil), body...)})
		}
		aligned := nlAlign(length)
		if aligned > len(data) {
			if length != len(data) {
				return false, fmt.Errorf("short netlink message alignment")
			}
			aligned = length // A final message need not include trailing padding.
		}
		data = data[aligned:]
	}
	if terminalErr != nil {
		return false, terminalErr
	}
	if s.dump || s.multipart {
		return s.done, nil
	}
	return s.ack || (!s.wantACK && len(s.replies) != 0), nil
}

func networkErrno(body []byte) error {
	code := int32(binary.NativeEndian.Uint32(body))
	if code > 0 || code < -4095 {
		return fmt.Errorf("invalid netlink error %d", code)
	}
	if code < 0 {
		return &networkKernelError{errno: unix.Errno(-int64(code))}
	}
	return nil
}

// Attributes are views into the payload, which remains owned by the reply.
// Duplicate values are rejected rather than choosing an arbitrary address,
// OIF or socket metadata. Empty attributes may repeat: the attribute number
// used for 64-bit alignment padding differs between netlink families.
func parseNetworkAttrs(data []byte) (map[uint16][]byte, error) {
	attrs := make(map[uint16][]byte)
	for len(data) != 0 {
		if len(data) < 4 {
			return nil, fmt.Errorf("short netlink attribute header")
		}
		length := int(binary.NativeEndian.Uint16(data))
		kind := binary.NativeEndian.Uint16(data[2:]) & 0x3fff
		if length < 4 || length > len(data) {
			return nil, fmt.Errorf("invalid netlink attribute length %d", length)
		}
		value := data[4:length]
		if previous, exists := attrs[kind]; exists && (len(previous) != 0 || len(value) != 0) {
			return nil, fmt.Errorf("duplicate netlink attribute %d", kind)
		}
		attrs[kind] = value
		aligned := nlAlign(length)
		if aligned > len(data) {
			if length != len(data) {
				return nil, fmt.Errorf("short netlink attribute alignment")
			}
			aligned = length
		}
		data = data[aligned:]
	}
	return attrs, nil
}
