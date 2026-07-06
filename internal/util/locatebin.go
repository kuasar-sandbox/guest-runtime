package util

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// LocateBinary resolves an executable, preferring a copy that ships next to
// the running binary over a $PATH-installed one.
func LocateBinary(name string) (string, error) {
	if name != "" && !strings.ContainsRune(name, os.PathSeparator) {
		if exe, err := os.Executable(); err == nil {
			candidate := filepath.Join(filepath.Dir(exe), name)
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("%q not found (tried exe-dir, $PATH)", name)
}
