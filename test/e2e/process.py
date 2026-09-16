#!/usr/bin/env python3
"""Run one E2E command and reap its entire Linux process tree on every exit."""

import argparse
import ctypes
import os
from pathlib import Path
import signal
import subprocess
import sys
import time


def descendants(pid):
    """Only walk our descendants, including children that created new sessions."""
    children = set()
    for task in Path(f"/proc/{pid}/task").glob("*"):
        try:
            children.update(int(value) for value in (task / "children").read_text().split())
        except FileNotFoundError:
            pass
    return [nested for child in children for nested in [*descendants(child), child]]


def signal_children(sig):
    for pid in descendants(os.getpid()):
        try:
            os.kill(pid, sig)
        except ProcessLookupError:
            pass


def reap():
    while True:
        try:
            if os.waitpid(-1, os.WNOHANG)[0] == 0:
                return True
        except ChildProcessError:
            return False


def supervise(command, *, parent=None, timeout=0, grace=2):
    stopped = 0

    def stop(sig, _frame):
        nonlocal stopped
        stopped = stopped or sig

    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, stop)
    libc = ctypes.CDLL(None, use_errno=True)
    # PR_SET_CHILD_SUBREAPER adopts orphaned grandchildren so they are reaped
    # even in a container whose PID 1 does not reap. PDEATHSIG closes the small
    # shell launch/trap race before the supervisor PID is added to its array.
    for option, value in ((36, 1), (1, signal.SIGTERM)):
        if libc.prctl(option, value, 0, 0, 0) != 0:
            raise OSError(ctypes.get_errno(), "prctl")
    if parent is not None and os.getppid() != parent:
        return 143
    child = None
    result = 1
    try:
        if stopped:
            return 128 + stopped
        child = subprocess.Popen(command, start_new_session=True)
        deadline = time.monotonic() + timeout if timeout else float("inf")
        while child.poll() is None and not stopped and time.monotonic() < deadline:
            time.sleep(0.02)
        if stopped:
            result = 128 + stopped
        elif child.returncode is None:
            print(f"e2e: command timed out: {command[0]}", file=sys.stderr)
            result = 124
        else:
            result = child.returncode if child.returncode >= 0 else 128 - child.returncode
    finally:
        # Stop descendants even when the main command exited during startup.
        # Re-scan after adoption/forks; never signal a process by name or a
        # saved PID that may have been reused after it was reaped.
        for sig in (signal.SIGTERM, signal.SIGKILL):
            deadline = time.monotonic() + grace
            while True:
                signal_children(sig)
                if child is not None:
                    child.poll()
                if not reap():
                    break
                if time.monotonic() >= deadline:
                    break
                time.sleep(0.02)
        if descendants(os.getpid()):
            print("e2e: could not reap all command descendants", file=sys.stderr)
            result = result or 1
    return result


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--parent", type=int)
    parser.add_argument("--timeout", type=float, default=0)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    if not command:
        parser.error("a command is required")
    try:
        sys.exit(supervise(command, parent=args.parent, timeout=args.timeout))
    except OSError as error:
        print(f"e2e: {error}", file=sys.stderr)
        sys.exit(1)
