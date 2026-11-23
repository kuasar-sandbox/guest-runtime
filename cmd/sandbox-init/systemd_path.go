package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
)

const (
	systemdPathMarker   = "_SANDBOX_RUNTIME_SYSTEMD_PATH"
	systemdLoginProfile = "/etc/profile.d/00-sandbox-runtime-systemd.sh"
)

func systemdPathMarkerValue(defaultPath bool) string {
	if defaultPath {
		return "1"
	}
	return "0"
}
func prependSystemdTools(path string) string {
	prefix := systemdPrefix + "/bin"
	if path == prefix || strings.HasPrefix(path, prefix+":") {
		return path
	}
	return prefix + ":" + path
}

// Standard login profiles often replace the inherited PATH. The internal
// marker carries trusted default provenance across that reset. An explicit
// caller PATH receives marker 0, even if it is textually equal to the default.
// User-owned shell startup files retain their ordinary Linux semantics.
func systemdLoginScript() string {
	return `# Runtime systemd tools for login shells that source /etc/profile.d.
if [ "${_SANDBOX_RUNTIME_SYSTEMD_PATH-0}" = 1 ] && [ -d /run/sandbox-runtime/systemd/bin ]; then
    case ":${PATH-}:" in
        *:/run/sandbox-runtime/systemd/bin:*) ;;
        *) PATH="/run/sandbox-runtime/systemd/bin${PATH+:$PATH}"; export PATH ;;
    esac
fi
`
}

func prepareSystemdLoginPath() error {
	// A previous E export may retain only the empty bind mountpoint. Existing
	// user content or a symlink at this reserved name is never overwritten.
	if st, err := os.Lstat(systemdLoginProfile); err == nil {
		if !st.Mode().IsRegular() || st.Size() != 0 {
			return fmt.Errorf("reserved login profile %s is already occupied", systemdLoginProfile)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return applyFiles([]proto.FileSpec{{Path: systemdLoginProfile, Content: systemdLoginScript(), Mode: "0644", Owner: "0:0", ReadOnly: true}})
}
