//go:build windows

package chromedp

import (
	"context"
	"testing"
)

// assertRecoveryBrowserStops ends the context. Windows has no signal 0 to test
// whether a process runs, so this version checks no process. The Unix file
// holds the version with the process check.
func assertRecoveryBrowserStops(t *testing.T, b *Browser, cancel context.CancelFunc) {
	t.Helper()
	cancel()
}
