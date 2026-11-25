package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const systemdSysinitDropIn = "[Unit]\nRequires=" + systemdService + "\nAfter=" + systemdService + "\n"

// prepareSystemdBootBarrier makes the executor READY notification a prerequisite
// for completing sysinit.target. Ordinary system services inherit this barrier
// through systemd's default dependencies on sysinit.target.
func prepareSystemdBootBarrier(unitDir string, otherUnitDirs ...string) error {
	const dropIn = "sysinit.target.d/50-sandbox-init.conf"
	for _, dir := range otherUnitDirs {
		if err := verifySystemdBootBarrier(filepath.Join(dir, dropIn)); err != nil {
			return err
		}
	}

	path := filepath.Join(unitDir, dropIn)
	if err := verifySystemdBootBarrier(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		// The exact runtime-owned file is already present.
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect systemd boot barrier %s: %w", path, err)
	}
	return writeReserved(path, systemdSysinitDropIn)
}

// verifySystemdBootBarrier accepts an existing copy only when it is the exact
// runtime-owned regular file. This catches lower-priority image drop-ins too,
// before the /run copy could silently shadow them.
func verifySystemdBootBarrier(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect systemd boot barrier %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("systemd boot barrier %s is not a regular file", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read systemd boot barrier %s: %w", path, err)
	}
	if string(content) != systemdSysinitDropIn {
		return fmt.Errorf("conflicting systemd boot barrier %s", path)
	}
	return nil
}
