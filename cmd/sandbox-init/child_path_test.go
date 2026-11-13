package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
)

// Exercise the production re-entry point when runInit starts /proc/self/exe.
func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == "init-command" {
		main()
	}
	os.Exit(m.Run())
}

func TestNonRootCommandPath(t *testing.T) {
	const name = "workload-command"
	const user = "65534:65534"
	if workdir := os.Getenv("SANDBOX_INIT_TEST_PATH_HELPER"); workdir != "" {
		if err := execFinalApplication(nil, user, workdir, name, nil, false, false); err != nil {
			t.Fatal(err)
		}
		t.Fatal("final exec returned")
	}
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise a real credential transition")
	}
	// t.TempDir has a private parent which a dropped child cannot traverse.
	root, err := os.MkdirTemp("", "sandbox-init-path-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	private, public := filepath.Join(root, "private"), filepath.Join(root, "public")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(public, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, name), []byte("#!/bin/sh\nexit 99\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(public, name), []byte("#!/bin/sh\nprintf ok > marker\n"), 0755); err != nil {
		t.Fatal(err)
	}
	path := private + ":" + public
	for _, mode := range []string{"application", "init"} {
		t.Run(mode, func(t *testing.T) {
			workdir := filepath.Join(root, mode)
			if err := os.Mkdir(workdir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(workdir, 65534, 65534); err != nil {
				t.Fatal(err)
			}
			if mode == "init" {
				spec := proto.InitSpec{Exec: name, User: user, Workdir: workdir, Env: map[string]string{"PATH": path}}
				if err := runInit([]proto.InitSpec{spec}); err != nil {
					t.Fatal(err)
				}
			} else {
				cmd := exec.Command(os.Args[0], "-test.run=^TestNonRootCommandPath$")
				cmd.Env = []string{"PATH=" + path, "SANDBOX_INIT_TEST_PATH_HELPER=" + workdir}
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("child: %v, %s", err, out)
				}
			}
			if got, err := os.ReadFile(filepath.Join(workdir, "marker")); err != nil || string(got) != "ok" {
				t.Fatalf("workdir marker: %q, %v", got, err)
			}
		})
	}
}
