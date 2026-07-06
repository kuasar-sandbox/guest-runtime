//go:build !linux

package main

import "errors"

func selfBind(string) error {
	return errors.New("mountpoint requires Linux")
}
