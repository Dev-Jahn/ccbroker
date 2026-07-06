//go:build windows

package main

import "fmt"

// startDetached is unsupported on Windows; the watch daemon targets macOS/Linux
// (launchd / systemd-user / nohup+cron).
func startDetached(exe, cfgPath, logPath string) error {
	return fmt.Errorf("ccb watch daemon is not supported on Windows")
}

// acquireWatchLock is a no-op on Windows (no flock; the watch daemon targets
// unix). It always "acquires" so a manually-run `ccb watch` still works, without
// the single-instance guarantee.
func acquireWatchLock(path string) (release func(), ok bool, err error) {
	return func() {}, true, nil
}
