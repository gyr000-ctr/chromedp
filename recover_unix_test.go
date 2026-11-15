//go:build !windows

package chromedp

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// assertRecoveryBrowserStops ends the context and makes sure that the browser
// process that a recovery started exits. On Windows a process has no signal 0,
// so the Windows file holds a version without the process check.
func assertRecoveryBrowserStops(t *testing.T, b *Browser, cancel context.CancelFunc) {
	t.Helper()
	if b.Process() == nil {
		t.Fatal("the new browser has no process")
	}
	pid := b.Process().Pid
	cancel()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the recovered browser process %d stayed open after the context ended", pid)
}
