package main

import (
	"bytes"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

type systemdELFABI struct {
	machine elf.Machine
	loader  string
}

func supportedSystemdELFABI(arch string) (systemdELFABI, error) {
	switch arch {
	case "amd64":
		return systemdELFABI{elf.EM_X86_64, "ld-linux-x86-64.so.2"}, nil
	case "arm64":
		return systemdELFABI{elf.EM_AARCH64, "ld-linux-aarch64.so.1"}, nil
	default:
		return systemdELFABI{}, fmt.Errorf("systemd manager architecture %q is not supported", arch)
	}
}

// inspectSystemdManager never executes candidate code in the outer environment.
// Its fixed poweroff signal is valid for the two qualified glibc ABIs only;
// static, musl and other loaders therefore fail before manager startup. This is
// a compatibility check, not authentication of an untrusted ELF producer.
// The running manager authenticates its version after namespace/bootstrap.
func inspectSystemdManager(path string) error {
	abi, err := supportedSystemdELFABI(runtime.GOARCH)
	if err != nil {
		return err
	}
	file, f, err := openSystemdELF(path, "manager")
	if err != nil {
		return err
	}
	defer file.Close()
	interp, err := systemdManagerInterpreter(f, abi)
	if err != nil {
		return err
	}
	return inspectSystemdInterpreter(interp, abi)
}

func openSystemdELF(path, role string) (*os.File, *elf.File, error) {
	// A malformed candidate can name a FIFO/device. Open without blocking,
	// then inspect and parse the same descriptor only if it is a regular file.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %q: %w", role, path, err)
	}
	fail := func(err error) (*os.File, *elf.File, error) {
		_ = file.Close()
		return nil, nil, err
	}
	st, err := file.Stat()
	if err != nil {
		return fail(fmt.Errorf("%s %q: %w", role, path, err))
	}
	if !st.Mode().IsRegular() || st.Mode()&0o111 == 0 {
		return fail(fmt.Errorf("%s %q is not an executable regular file", role, path))
	}
	f, err := elf.NewFile(file)
	if err != nil {
		return fail(fmt.Errorf("%s ELF: %w", role, err))
	}
	return file, f, nil
}

func checkSystemdELFArchitecture(f *elf.File, abi systemdELFABI) error {
	if f.Machine != abi.machine || f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB {
		return fmt.Errorf("ELF architecture %s/%s/%s does not match %s/ELF64/little-endian",
			f.Machine, f.Class, f.Data, abi.machine)
	}
	return nil
}

func systemdManagerInterpreter(f *elf.File, abi systemdELFABI) (string, error) {
	if err := checkSystemdELFArchitecture(f, abi); err != nil {
		return "", fmt.Errorf("manager: %w", err)
	}
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return "", fmt.Errorf("manager ELF type %s is not executable", f.Type)
	}
	var interp *elf.Prog
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		if interp != nil {
			return "", errors.New("manager has multiple ELF interpreters")
		}
		interp = p
	}
	if interp == nil {
		return "", errors.New("manager requires a qualified glibc dynamic interpreter; static ELF is unsupported")
	}
	if interp.Filesz < 2 || interp.Filesz > 4096 {
		return "", errors.New("manager interpreter path has an invalid length")
	}
	b := make([]byte, interp.Filesz)
	if _, err := io.ReadFull(interp.Open(), b); err != nil {
		return "", fmt.Errorf("manager interpreter: %w", err)
	}
	if b[len(b)-1] != 0 || bytes.IndexByte(b[:len(b)-1], 0) >= 0 {
		return "", errors.New("manager interpreter requires a single terminal NUL")
	}
	path := string(b[:len(b)-1])
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("manager interpreter must be a clean absolute path")
	}
	// Match the ABI name, not a distribution's directory. Runtime projects the
	// same glibc loader under its private /run/sandbox-runtime/systemd prefix.
	if filepath.Base(path) != abi.loader {
		return "", fmt.Errorf("manager interpreter %q is not the qualified glibc loader %s", path, abi.loader)
	}
	libraries, err := f.ImportedLibraries()
	if err != nil {
		return "", fmt.Errorf("manager dynamic libraries: %w", err)
	}
	var libc bool
	for _, name := range libraries {
		libc = libc || name == "libc.so.6"
	}
	if !libc {
		return "", errors.New("manager requires the glibc libc.so.6 dependency")
	}
	symbols, err := f.ImportedSymbols()
	if err != nil {
		return "", fmt.Errorf("manager dynamic symbols: %w", err)
	}
	for _, symbol := range symbols {
		if symbol.Library == "libc.so.6" && strings.HasPrefix(symbol.Version, "GLIBC_") {
			return path, nil
		}
	}
	return "", errors.New("manager requires versioned GLIBC imports from libc.so.6")
}

func inspectSystemdInterpreter(path string, abi systemdELFABI) error {
	file, f, err := openSystemdELF(path, "manager interpreter")
	if err != nil {
		return err
	}
	defer file.Close()
	if err := checkSystemdELFArchitecture(f, abi); err != nil {
		return fmt.Errorf("manager interpreter: %w", err)
	}
	if f.Type != elf.ET_DYN {
		return fmt.Errorf("manager interpreter ELF type %s is not a dynamic loader", f.Type)
	}
	names, err := f.DynString(elf.DT_SONAME)
	if err != nil {
		return fmt.Errorf("manager interpreter SONAME: %w", err)
	}
	if len(names) != 1 || names[0] != abi.loader {
		return fmt.Errorf("manager interpreter SONAME %q does not match %s", names, abi.loader)
	}
	versions, err := f.DynamicVersions()
	if err != nil {
		return fmt.Errorf("manager interpreter glibc versions: %w", err)
	}
	for _, version := range versions {
		if version.Name == "GLIBC_PRIVATE" {
			return nil
		}
	}
	return errors.New("manager interpreter is missing the GLIBC_PRIVATE loader ABI")
}
