package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
)

// Exercise production re-entry points when a launch helper starts /proc/self/exe.
func TestMain(m *testing.M) {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "init-command", "exec-child", "plugin-child", "exec-join", "exec-child-joined":
			main()
		case "test-execution-context-outer", "test-execution-context-inner", "test-execution-context-probe":
			os.Exit(runExecutionContextHelper())
		}
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
				if err := runPreparedInitForTest(spec); err != nil {
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
