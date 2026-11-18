package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// prepareInnerEnvironment runs once in the inner init, before user init and
// before it reports ready to the outer init. The fork child has already entered
// /sandbox and created the PID, mount and cgroup namespaces. No outer-global
// cgroup descriptor is inherited: the inner opens its own scoped capabilities.
// The outer enables the environment's controllers only after this returns and
// confirms that bootstrap completed, so no user code can race that boundary.
func prepareInnerEnvironment(control, processGroups bool) error {
	if os.Getpid() != 1 {
		return fmt.Errorf("inner environment requires PID 1, got %d", os.Getpid())
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := makeChildMountsRSlave(); err != nil {
		return err
	}
	if err := remountChildProc(); err != nil {
		return err
	}
	if err := cgroupMountScoped(); err != nil {
		return err
	}
	if control {
		if err := cgroupMoveBootstrapToInit(); err != nil {
			return err
		}
	}
	return cgroupOpenInner(control, processGroups)
}

func cgroupOpenInner(control, processGroups bool) error {
	root, err := openCgroupDir(cgroupMountPoint)
	if err != nil {
		return fmt.Errorf("open inner cgroup root: %w", err)
	}
	targetPath := cgroupMountPoint
	if control {
		targetPath += "/init"
	}
	target, err := openCgroupDir(targetPath)
	if err != nil {
		_ = root.Close()
		return fmt.Errorf("open inner cgroup target: %w", err)
	}
	nsFD, err := unix.Open("/proc/self/ns/cgroup", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = root.Close()
		_ = target.Close()
		return fmt.Errorf("pin inner cgroup namespace: %w", err)
	}
	namespace := os.NewFile(uintptr(nsFD), "inner-cgroup-namespace")
	applicationCgroups.Lock()
	defer applicationCgroups.Unlock()
	if applicationCgroups.root != nil || applicationCgroups.target != nil || applicationCgroups.namespace != nil {
		_ = root.Close()
		_ = target.Close()
		_ = namespace.Close()
		return errors.New("inner cgroup capabilities already initialized")
	}
	applicationCgroups.control = control
	applicationCgroups.processGroups = processGroups
	applicationCgroups.root = root
	applicationCgroups.target = target
	applicationCgroups.namespace = namespace
	return nil
}
