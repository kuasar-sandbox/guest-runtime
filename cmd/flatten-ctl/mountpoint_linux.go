//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

// selfBind creates dir and bind-mounts it onto itself.
func selfBind(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := syscall.Mount(dir, dir, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s onto itself: %w", dir, err)
	}
	return nil
}
