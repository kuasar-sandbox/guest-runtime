package main

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Use an actual dynamically linked executable so mutations exercise the ELF
// reader, version tables and interpreter lookup together. Static Go test
// binaries cannot serve as qualified manager fixtures.
func systemdGlibcFixture(t *testing.T) ([]byte, []byte, systemdELFABI) {
	t.Helper()
	abi, err := supportedSystemdELFABI(runtime.GOARCH)
	if err != nil {
		t.Skipf("no qualified systemd ABI on %s: %v", runtime.GOARCH, err)
	}
	b, err := os.ReadFile("/bin/sh")
	if err != nil {
		t.Skipf("native dynamic /bin/sh fixture unavailable: %v", err)
	}
	f, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		t.Skipf("native /bin/sh is not an ELF fixture: %v", err)
	}
	var interp string
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			raw := make([]byte, p.Filesz)
			if _, err := p.ReadAt(raw, 0); err != nil {
				t.Fatal(err)
			}
			interp = strings.TrimSuffix(string(raw), "\x00")
		}
	}
	if interp == "" || filepath.Base(interp) != abi.loader {
		t.Skipf("native /bin/sh uses static or non-glibc interpreter %q; requires %s", interp, abi.loader)
	}
	loader, err := os.ReadFile(interp)
	if err != nil {
		t.Fatalf("read native glibc interpreter: %v", err)
	}
	lf, err := elf.NewFile(bytes.NewReader(loader))
	if err != nil {
		t.Fatalf("parse native glibc interpreter: %v", err)
	}
	versions, err := lf.DynamicVersions()
	if err != nil {
		t.Skipf("native interpreter %s has no glibc version definitions: %v", interp, err)
	}
	for _, version := range versions {
		if version.Name == "GLIBC_PRIVATE" {
			return b, loader, abi
		}
	}
	t.Skipf("native interpreter %s is not a qualified glibc fixture (no GLIBC_PRIVATE)", interp)
	return nil, nil, systemdELFABI{}
}

func systemdTestELF(t *testing.T, b []byte) *elf.File {
	t.Helper()
	f, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func systemdTestProgramHeaders(t *testing.T, b []byte) []int {
	t.Helper()
	if len(b) < 64 || b[4] != byte(elf.ELFCLASS64) || b[5] != byte(elf.ELFDATA2LSB) {
		t.Fatal("fixture is not ELF64 little-endian")
	}
	offset := binary.LittleEndian.Uint64(b[32:40])
	size := int(binary.LittleEndian.Uint16(b[54:56]))
	count := int(binary.LittleEndian.Uint16(b[56:58]))
	if size != 56 || offset > uint64(len(b)) || count > (len(b)-int(offset))/size {
		t.Fatal("invalid fixture program headers")
	}
	result := make([]int, count)
	for i := range result {
		result[i] = int(offset) + i*size
	}
	return result
}

func systemdTestInterpHeader(t *testing.T, b []byte) int {
	t.Helper()
	for _, offset := range systemdTestProgramHeaders(t, b) {
		if binary.LittleEndian.Uint32(b[offset:offset+4]) == uint32(elf.PT_INTERP) {
			return offset
		}
	}
	t.Fatal("fixture has no interpreter")
	return 0
}

func systemdTestSetInterpreter(t *testing.T, b, path []byte) []byte {
	t.Helper()
	b = bytes.Clone(b)
	offset := systemdTestInterpHeader(t, b)
	binary.LittleEndian.PutUint64(b[offset+8:offset+16], uint64(len(b)))
	binary.LittleEndian.PutUint64(b[offset+32:offset+40], uint64(len(path)))
	binary.LittleEndian.PutUint64(b[offset+40:offset+48], uint64(len(path)))
	return append(b, path...)
}

func systemdTestReplaceDynamicString(t *testing.T, b []byte, old, replacement string) []byte {
	t.Helper()
	if len(old) != len(replacement) {
		t.Fatal("dynamic string mutation must preserve offsets")
	}
	b = bytes.Clone(b)
	f := systemdTestELF(t, b)
	section := f.SectionByType(elf.SHT_DYNAMIC)
	if section == nil || int(section.Link) >= len(f.Sections) {
		t.Fatal("fixture has no dynamic string table")
	}
	stringsSection := f.Sections[section.Link]
	data := b[stringsSection.Offset : stringsSection.Offset+stringsSection.Size]
	if !bytes.Contains(data, []byte(old)) {
		t.Fatalf("fixture has no dynamic string %q", old)
	}
	copy(data, bytes.ReplaceAll(data, []byte(old), []byte(replacement)))
	return b
}

func systemdTestWriteELF(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestSystemdELFQualifiedDynamicManager(t *testing.T) {
	manager, loader, abi := systemdGlibcFixture(t)
	for _, layout := range []string{"native", "distribution-symlink", "run/sandbox-runtime/systemd/lib"} {
		t.Run(layout, func(t *testing.T) {
			dir := t.TempDir()
			b := manager
			if layout != "native" {
				interp := filepath.Join(dir, layout, abi.loader)
				if layout == "distribution-symlink" {
					realLoader := filepath.Join(dir, "lib", "ld-qualified.so")
					systemdTestWriteELF(t, realLoader, loader)
					if err := os.MkdirAll(filepath.Dir(interp), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(realLoader, interp); err != nil {
						t.Fatal(err)
					}
				} else {
					systemdTestWriteELF(t, interp, loader)
				}
				b = systemdTestSetInterpreter(t, b, append([]byte(interp), 0))
			}
			path := filepath.Join(dir, "systemd")
			systemdTestWriteELF(t, path, b)
			if err := inspectSystemdManager(path); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSystemdELFRejectsUnqualifiedManager(t *testing.T) {
	manager, _, abi := systemdGlibcFixture(t)
	cases := []struct {
		name   string
		mutate func(*testing.T, []byte) []byte
		want   string
	}{
		{"static", func(t *testing.T, b []byte) []byte {
			offset := systemdTestInterpHeader(t, b)
			binary.LittleEndian.PutUint32(b[offset:offset+4], uint32(elf.PT_NULL))
			return b
		}, "static ELF is unsupported"},
		{"multiple-interpreters", func(t *testing.T, b []byte) []byte {
			interp := systemdTestInterpHeader(t, b)
			for _, offset := range systemdTestProgramHeaders(t, b) {
				if binary.LittleEndian.Uint32(b[offset:offset+4]) == uint32(elf.PT_NOTE) {
					copy(b[offset:offset+56], b[interp:interp+56])
					return b
				}
			}
			t.Fatal("fixture has no spare PT_NOTE")
			return nil
		}, "multiple ELF interpreters"},
		{"non-executable-type", func(t *testing.T, b []byte) []byte {
			binary.LittleEndian.PutUint16(b[16:18], uint16(elf.ET_CORE))
			return b
		}, "not executable"},
		{"wrong-architecture", func(t *testing.T, b []byte) []byte {
			binary.LittleEndian.PutUint16(b[18:20], uint16(elf.EM_RISCV))
			return b
		}, "architecture"},
		{"no-libc-dependency", func(t *testing.T, b []byte) []byte {
			return systemdTestReplaceDynamicString(t, b, "libc.so.6", "libx.so.6")
		}, "libc.so.6 dependency"},
		{"no-glibc-imports", func(t *testing.T, b []byte) []byte {
			return systemdTestReplaceDynamicString(t, b, "GLIBC_", "OTHER_")
		}, "versioned GLIBC imports"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.mutate(t, bytes.Clone(manager))
			path := filepath.Join(t.TempDir(), "systemd")
			systemdTestWriteELF(t, path, b)
			if err := inspectSystemdManager(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if _, err := findSystemdCandidates([]string{path}); err == nil {
				t.Fatal("present incompatible candidate was treated as absent")
			}
		})
	}
	for _, tc := range []struct {
		name string
		path []byte
		want string
	}{
		{"empty", nil, "invalid length"},
		{"nul-only", []byte{0}, "invalid length"},
		{"too-long", append(bytes.Repeat([]byte{'a'}, 4096), 0), "invalid length"},
		{"no-terminal-nul", []byte("/lib/" + abi.loader), "single terminal NUL"},
		{"embedded-nul", []byte("/lib/\x00" + abi.loader + "\x00"), "single terminal NUL"},
		{"duplicate-terminal-nul", []byte("/lib/" + abi.loader + "\x00\x00"), "single terminal NUL"},
		{"relative", []byte("lib/" + abi.loader + "\x00"), "clean absolute path"},
		{"unclean", []byte("/lib/../lib/" + abi.loader + "\x00"), "clean absolute path"},
		{"musl", []byte("/lib/ld-musl-" + runtime.GOARCH + ".so.1\x00"), "qualified glibc loader"},
		{"other-loader", []byte("/lib/ld-other.so\x00"), "qualified glibc loader"},
	} {
		t.Run("interpreter-"+tc.name, func(t *testing.T) {
			b := systemdTestSetInterpreter(t, manager, tc.path)
			path := filepath.Join(t.TempDir(), "systemd")
			systemdTestWriteELF(t, path, b)
			if err := inspectSystemdManager(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSystemdELFRejectsUnqualifiedLoader(t *testing.T) {
	manager, loader, abi := systemdGlibcFixture(t)
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, []byte) []byte
		want   string
	}{
		{"non-elf", func(t *testing.T, b []byte) []byte { return []byte("not a loader\n") }, "ELF"},
		{"wrong-architecture", func(t *testing.T, b []byte) []byte {
			binary.LittleEndian.PutUint16(b[18:20], uint16(elf.EM_RISCV))
			return b
		}, "architecture"},
		{"non-dynamic", func(t *testing.T, b []byte) []byte {
			binary.LittleEndian.PutUint16(b[16:18], uint16(elf.ET_EXEC))
			return b
		}, "not a dynamic loader"},
		{"wrong-soname", func(t *testing.T, b []byte) []byte {
			return systemdTestReplaceDynamicString(t, b, abi.loader, "xx"+abi.loader[2:])
		}, "SONAME"},
		{"no-glibc-version", func(t *testing.T, b []byte) []byte {
			return systemdTestReplaceDynamicString(t, b, "GLIBC_PRIVATE", "OTHER_PRIVATE")
		}, "GLIBC_PRIVATE"},
		{"glibc-name-with-another-elf", func(t *testing.T, b []byte) []byte { return manager }, "SONAME"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			interp := filepath.Join(dir, "lib", abi.loader)
			systemdTestWriteELF(t, interp, tc.mutate(t, bytes.Clone(loader)))
			path := filepath.Join(dir, "systemd")
			systemdTestWriteELF(t, path, systemdTestSetInterpreter(t, manager, append([]byte(interp), 0)))
			if err := inspectSystemdManager(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
	for _, kind := range []string{"missing", "not-executable", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			interp := filepath.Join(dir, abi.loader)
			switch kind {
			case "not-executable":
				if err := os.WriteFile(interp, loader, 0o644); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(interp, 0o755); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(interp, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dir, "systemd")
			systemdTestWriteELF(t, path, systemdTestSetInterpreter(t, manager, append([]byte(interp), 0)))
			if err := inspectSystemdManager(path); err == nil {
				t.Fatalf("accepted %s interpreter", kind)
			}
		})
	}
}

func TestSystemdELFRejectsUnsupportedArchitecture(t *testing.T) {
	for _, arch := range []string{"386", "riscv64", ""} {
		if _, err := supportedSystemdELFABI(arch); err == nil {
			t.Fatalf("unsupported ABI %q silently used the x86 loader", arch)
		}
	}
}
