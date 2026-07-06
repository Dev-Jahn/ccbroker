//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// startDetached launches `ccb watch -c cfgPath` in its own session (setsid) so
// it outlives the cron `ensure-alive` wrapper that started it. Output is
// appended to logPath.
func startDetached(exe, cfgPath, logPath string) error {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command(exe, "watch", "-c", cfgPath)
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

// acquireWatchLock takes an exclusive advisory lock (flock) on the pidfile and
// holds it via the returned release closure (call it when the process exits). It
// is the single-instance guard for `ccb watch` (MAJOR-4): ok=false means another
// process already holds the lock — a live watcher — regardless of the file's
// contents. The lock is released automatically when the process dies (the kernel
// drops flocks on close/exit), so a crashed watcher never wedges the slot.
func acquireWatchLock(path string) (release func(), ok bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, false, nil // held by a live watcher
		}
		return nil, false, err
	}
	// We hold the lock. Record our pid for inspection (the lock, not the content,
	// is the source of truth).
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	fmt.Fprintf(f, "%d\n", os.Getpid())
	_ = f.Sync()
	release = func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	return release, true, nil
}
