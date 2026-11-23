package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

const environmentExitNotifyBudget = time.Second

// Unlike initial Launch retry, termination has one deadline covering dial,
// message and ACK. It fits inside the Host's final cleanup allowance.
func notifyEnvironmentExit(result proto.EnvironmentExit) error {
	deadline := time.Now().Add(environmentExitNotifyBudget)
	conn, err := dialEnvironmentExit(deadline)
	if err != nil {
		return err
	}
	defer conn.Close()
	return exchangeEnvironmentExit(conn, result, deadline)
}

func dialEnvironmentExit(deadline time.Time) (*vsockConn, error) {
	var lastErr error
	for time.Now().Before(deadline) {
		fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
		if err != nil {
			return nil, fmt.Errorf("environment exit socket: %w", err)
		}
		err = unix.Connect(fd, &unix.SockaddrVM{CID: proto.VsockHostCID, Port: proto.LaunchPort})
		if errors.Is(err, unix.EINPROGRESS) {
			for time.Now().Before(deadline) {
				remaining := time.Until(deadline)
				poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
				var n int
				n, err = unix.Poll(poll, int((remaining+time.Millisecond-1)/time.Millisecond))
				if errors.Is(err, unix.EINTR) {
					continue
				}
				if err == nil && n == 0 {
					err = unix.ETIMEDOUT
				}
				if err == nil {
					var socketErr int
					socketErr, err = unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
					if err == nil && socketErr != 0 {
						err = unix.Errno(socketErr)
					}
				}
				break
			}
		}
		if err == nil && time.Now().Before(deadline) {
			if err = unix.SetNonblock(fd, false); err == nil {
				return &vsockConn{fd: fd}, nil
			}
		}
		_ = unix.Close(fd)
		lastErr = err
		if remaining := time.Until(deadline); remaining > 0 {
			time.Sleep(min(vsockDialMax, remaining))
		}
	}
	return nil, fmt.Errorf("environment exit dial deadline exceeded: %v", lastErr)
}

func exchangeEnvironmentExit(conn interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	SetDeadline(time.Time) error
	Close() error
}, result proto.EnvironmentExit, deadline time.Time) error {
	timerDone := make(chan struct{})
	timer := time.AfterFunc(time.Until(deadline), func() {
		defer close(timerDone)
		if c, ok := conn.(*vsockConn); ok {
			_ = c.shutdown()
		}
		_ = conn.Close()
	})
	defer func() {
		if !timer.Stop() {
			<-timerDone
		}
	}()
	_ = conn.SetDeadline(deadline)
	if err := proto.WriteMessage(conn, &proto.Message{Type: proto.TypeEnvironmentExited, EnvironmentExit: &result}); err != nil {
		return fmt.Errorf("environment exit write: %w", err)
	}
	response, err := proto.ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("environment exit acknowledgement: %w", err)
	}
	if response.Type != proto.TypeAck {
		return fmt.Errorf("unexpected environment exit response %q", response.Type)
	}
	return nil
}
