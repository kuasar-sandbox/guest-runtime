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
func prepareSystemdBootBarrier(unitDir string) error {
	path := filepath.Join(unitDir, "sysinit.target.d", "50-sandbox-init.conf")
	if err := writeReserved(path, systemdSysinitDropIn); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}

	// Repeated preparation is safe only when the reserved drop-in already has
	// exactly the content owned by this runtime. Never silently replace a
	// conflicting file or follow a non-regular path.
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect reserved systemd boot barrier %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("reserved systemd boot barrier %s is not a regular file", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read reserved systemd boot barrier %s: %w", path, err)
	}
	if string(content) != systemdSysinitDropIn {
		return fmt.Errorf("conflicting systemd boot barrier %s", path)
	}
	return nil
}
