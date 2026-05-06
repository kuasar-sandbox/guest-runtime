package sandbox

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/fullof-work/mass-sandbox/pkg/flatten"
	"github.com/fullof-work/mass-sandbox/pkg/sandbox/proto"
)

// ImageConfig captures the container default launch settings the
// flatten-ctl tool appends to the rootfs erofs as a trailing ZIP entry
// (config.json). When sandbox-ctl prepares the launch spec for sandbox-
// init, it merges these defaults with the sandbox.yaml `launch:` overrides.
//
// The fields mirror the OCI image config subset we honor.
type ImageConfig struct {
	Cmd        []string
	Entrypoint []string
	Env        []string // "K=V" form, OCI-native
	WorkingDir string
}

// LoadImageConfig reads the trailing ZIP from the rootfs erofs at
// path and returns the parsed config. Convenience wrapper for the
// file:// disk URI path — distinguishes "file missing" (hard error)
// from "file present, no ZIP" (soft, empty config returned).
//
// manifest:// callers should use LoadImageConfigFrom directly with a
// fetch.Fetcher-backed io.ReaderAt — bytes don't need to round-trip
// through a temp file.
func LoadImageConfig(path string) (*ImageConfig, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("load image config: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("load image config: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("load image config: %w", err)
	}
	return LoadImageConfigFrom(f, st.Size())
}

// LoadImageConfigFrom is the canonical reader: takes an io.ReaderAt
// of any source (regular file, fetch.Fetcher-backed manifest reader,
// in-memory buffer) and returns the parsed image config or an empty
// one when no trailer is present.
//
// Soft-failure semantics: missing ZIP trailer or missing config.json
// entry → empty config + nil error so callers can run MergeLaunch
// unconditionally. Hard errors (malformed JSON, transport failure)
// propagate.
//
// Delegates to flatten.ReadConfig, the authoritative reader paired
// with flatten-ctl's AppendConfigZip writer. We translate
// flatten.RuntimeConfig (persisted shape) into our internal
// ImageConfig (merge-step shape).
func LoadImageConfigFrom(r io.ReaderAt, size int64) (*ImageConfig, error) {
	rc, err := flatten.ReadConfig(r, size)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &ImageConfig{}, nil
		}
		return nil, fmt.Errorf("load image config: %w", err)
	}
	return &ImageConfig{
		Cmd:        rc.Cmd,
		Entrypoint: rc.Entrypoint,
		Env:        rc.Env,
		WorkingDir: rc.WorkingDir,
	}, nil
}

// MergeLaunch combines image defaults with the sandbox.yaml launch
// override and returns the spec to send to sandbox-init.
//
// Precedence follows Docker's --entrypoint / args semantics:
//
//   - override.Exec set → replaces both ENTRYPOINT and CMD.
//     Resulting argv = [override.Exec] ++ override.Args.
//   - else image.Entrypoint non-empty → exec is image.Entrypoint[0].
//     If override.Args set, it replaces image.Cmd portion; otherwise
//     args = image.Entrypoint[1:] ++ image.Cmd.
//   - else image.Cmd non-empty → exec is image.Cmd[0]. If override.Args
//     set, it replaces image.Cmd[1:]; otherwise args = image.Cmd[1:].
//   - else → error (no executable resolvable).
//
// Env: image first, override on top (override keys win).
// Workdir / Restart: override else image else default.
func MergeLaunch(image *ImageConfig, override LaunchConfig) (*proto.LaunchSpec, error) {
	spec := &proto.LaunchSpec{
		Workdir: "/",
		Restart: "never",
	}

	switch {
	case override.Exec != "":
		spec.Exec = override.Exec
		spec.Args = override.Args

	case image != nil && len(image.Entrypoint) > 0:
		spec.Exec = image.Entrypoint[0]
		entryRest := image.Entrypoint[1:]
		if len(override.Args) > 0 {
			spec.Args = append(append([]string{}, entryRest...), override.Args...)
		} else {
			spec.Args = append(append([]string{}, entryRest...), image.Cmd...)
		}

	case image != nil && len(image.Cmd) > 0:
		spec.Exec = image.Cmd[0]
		if len(override.Args) > 0 {
			spec.Args = override.Args
		} else {
			spec.Args = append([]string{}, image.Cmd[1:]...)
		}

	default:
		return nil, errMissingExec
	}

	// Env: image first, override on top.
	envMap := map[string]string{}
	if image != nil {
		for _, kv := range image.Env {
			if i := indexByte(kv, '='); i > 0 {
				envMap[kv[:i]] = kv[i+1:]
			}
		}
	}
	for k, v := range override.Env {
		envMap[k] = v
	}
	if len(envMap) > 0 {
		spec.Env = envMap
	}

	if override.Workdir != "" {
		spec.Workdir = override.Workdir
	} else if image != nil && image.WorkingDir != "" {
		spec.Workdir = image.WorkingDir
	}

	if override.Restart != "" {
		spec.Restart = override.Restart
	}

	return spec, nil
}

var errMissingExec = errMessage("merge launch: no executable found — set sandbox.yaml launch.exec, or ensure the rootfs image's config.json has Entrypoint/Cmd")

type errMessage string

func (e errMessage) Error() string { return string(e) }

// indexByte is a small helper to avoid importing strings into this file.
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
