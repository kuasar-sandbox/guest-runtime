package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

// Captured by outer init before user init/exec. Snapshot memory preserves this
// one binding; names and indices supplied again on restore are not new owners.
// A sysfs object identity also distinguishes a removed/recreated interface.
type platformNetwork struct {
	device       os.FileInfo
	netns        os.FileInfo // nil when the production kernel has NET_NS=n
	index        int32
	defaultRoute []byte
}

var guestNetwork platformNetwork

func readIfindex(name string) (int32, error) {
	data, err := os.ReadFile(filepath.Join("/sys/class/net", name, "ifindex"))
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 32)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid ifindex for %q: %q", name, data)
	}
	return int32(n), nil
}

func currentNetworkNamespace() (os.FileInfo, error) {
	info, err := os.Stat("/proc/self/ns/net")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return info, err
}

func sameNetworkNamespace(a, b os.FileInfo) bool {
	return a == nil && b == nil || a != nil && b != nil && os.SameFile(a, b)
}

func bindPlatformNetwork(spec *proto.NetworkSpec) error {
	ns, err := currentNetworkNamespace()
	if err != nil {
		return err
	}
	p := platformNetwork{netns: ns}
	name := ""
	if spec != nil {
		name = spec.Interface
	}
	if name == "" {
		name = "eth0"
	}
	if name != filepath.Base(name) || name == "." || name == ".." {
		return errors.New("invalid platform interface name")
	}
	path := filepath.Join("/sys/class/net", name)
	device, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) && (spec == nil || spec.IPCIDR == "") {
		guestNetwork = p
		return nil
	}
	if err != nil {
		return fmt.Errorf("bind platform interface %s: %w", name, err)
	}
	// sandboxer exposes one virtio-net NIC. No name-only inference of user
	// veth/tun/dummy devices, and no traversal into user network namespaces.
	driver, err := filepath.EvalSymlinks(filepath.Join(path, "device/driver"))
	if err != nil || filepath.Base(driver) != "virtio_net" {
		if spec == nil || spec.IPCIDR == "" {
			guestNetwork = p
			return nil
		}
		return fmt.Errorf("platform interface %s is not virtio-net", name)
	}
	index, err := readIfindex(name)
	if err != nil {
		return err
	}
	p.device, p.index = device, index
	guestNetwork = p
	return nil
}

func (p *platformNetwork) resolve(ctx context.Context, c networkRequester) (int32, error) {
	if p.device == nil {
		return 0, nil
	}
	ns, err := currentNetworkNamespace()
	if err != nil {
		return 0, err
	}
	if !sameNetworkNamespace(p.netns, ns) {
		return 0, errors.New("platform network namespace changed")
	}
	// Same-device rename is allowed. The number of other user interfaces is
	// irrelevant; only this one original kernel/sysfs object is selected.
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return 0, err
	}
	found := false
	for _, entry := range entries {
		info, err := os.Stat(filepath.Join("/sys/class/net", entry.Name()))
		if err == nil && os.SameFile(p.device, info) {
			index, err := readIfindex(entry.Name())
			if err != nil || index != p.index {
				return 0, errors.New("platform interface identity changed")
			}
			found = true
			break
		}
	}
	if !found {
		return 0, errors.New("platform interface is no longer in the original namespace")
	}
	body := make([]byte, unix.SizeofIfInfomsg)
	binary.NativeEndian.PutUint32(body[4:], uint32(p.index))
	replies, err := c.request(ctx, unix.RTM_GETLINK, unix.NLM_F_ACK, body)
	if err != nil {
		return 0, err
	}
	if len(replies) != 1 || replies[0].Type != unix.RTM_NEWLINK || len(replies[0].Data) < unix.SizeofIfInfomsg {
		return 0, errors.New("invalid platform link reply")
	}
	link := replies[0].Data
	if int32(binary.NativeEndian.Uint32(link[4:])) != p.index {
		return 0, errors.New("platform link reply changed identity")
	}
	attrs, err := parseNetworkAttrs(link[unix.SizeofIfInfomsg:])
	if err != nil {
		return 0, err
	}
	if master, ok := attrs[unix.IFLA_MASTER]; ok {
		if len(master) != 4 || binary.NativeEndian.Uint32(master) != 0 {
			return 0, errors.New("platform interface belongs to a user master device")
		}
	}
	return p.index, nil
}
