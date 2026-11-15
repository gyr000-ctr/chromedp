package chromedp

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
)

// crashPage crashes the renderer process of the target with Page.crash. The
// call ends with ErrTargetCrashed by design, so the action returns that
// error.
func crashPage() Action[Void] {
	return Func(func(ctx context.Context, t *Target) error {
		_, err := cdp.Call(ctx, t, page.Crash, cdp.Empty{})
		return err
	})
}

// recoveryTestPolicy returns a policy with short waits for the tests. The
// limits stay high enough for the cases that rebuild more than once.
func recoveryTestPolicy() RecoveryPolicy {
	p := DefaultRecoveryPolicy()
	p.InitialBackoff = 10 * time.Millisecond
	p.MaxBackoff = 50 * time.Millisecond
	p.Timeout = 30 * time.Second
	return p
}

// TestRecoveryTabCrashRebuildsTarget makes sure that a policy opens a new
// target after the renderer process stops, and that a later action works.
func TestRecoveryTabCrashRebuildsTarget(t *testing.T) {
	t.Parallel()
	ctx, cancel := NewContext(allocCtx, WithRecovery(recoveryTestPolicy()))
	defer cancel()

	pageURL := testdataDir + "/console.html"
	if err := Do(ctx, Navigate(pageURL)); err != nil {
		t.Fatal(err)
	}
	first := FromContext(ctx).Target

	// The first call sees the crash. The policy rebuilds the target, but the
	// action was not marked idempotent, so the call returns the crash error.
	err := Do(ctx, crashPage())
	if !errors.Is(err, ErrTargetCrashed) {
		t.Fatalf("want ErrTargetCrashed, got %v", err)
	}

	second := FromContext(ctx).Target
	if second == first {
		t.Fatal("the context still holds the target that crashed")
	}

	// The next call works on the new target. The restored page needs a moment
	// to load, so poll until its title is ready.
	var title string
	if _, err := Run(ctx, pollTitle(&title)); err != nil {
		t.Fatalf("the action after the recovery failed: %v", err)
	}
	if title == "" {
		t.Fatal("the new target returned an empty title")
	}
	got, err := Run(ctx, Location())
	if err != nil {
		t.Fatal(err)
	}
	if got != pageURL {
		t.Fatalf("want the restored URL %q, got %q", pageURL, got)
	}
}

// pollTitle is an action that waits until Title returns a non-empty value and
// stores it in dst. It gives the restored page ten seconds to load.
func pollTitle(dst *string) Action[Void] {
	return Func(func(ctx context.Context, t *Target) error {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return retryWithSleep(pctx, 50*time.Millisecond, func(ctx context.Context) (bool, error) {
			v, err := Title()(ctx, t)
			if err != nil {
				return false, err
			}
			if v == "" {
				return false, nil
			}
			*dst = v
			return true, nil
		})
	})
}

// TestRecoveryIdempotentRetriesAction makes sure that Run runs an action marked
// with Idempotent again on the new target, and that the caller sees no error.
func TestRecoveryIdempotentRetriesAction(t *testing.T) {
	t.Parallel()
	policy := recoveryTestPolicy()
	policy.RetryActions = true
	ctx, cancel := NewContext(allocCtx, WithRecovery(policy))
	defer cancel()

	if err := Do(ctx, Navigate(testdataDir+"/console.html")); err != nil {
		t.Fatal(err)
	}

	var runs atomic.Int32
	readTitle := Func(func(ctx context.Context, t *Target) error {
		if runs.Add(1) == 1 {
			_, _ = cdp.Call(ctx, t, page.Crash, cdp.Empty{})
			return ErrTargetCrashed
		}
		return nil
	})
	if err := Do(ctx, Idempotent(readTitle)); err != nil {
		t.Fatalf("the idempotent action did not recover: %v", err)
	}
	if runs.Load() != 2 {
		t.Fatalf("want 2 runs of the action, got %d", runs.Load())
	}
}

// TestRecoveryNoRetryWithoutMark makes sure that a policy with RetryActions
// still does not run an action again when the action has no Idempotent mark.
// This protects callers from duplicate side effects.
func TestRecoveryNoRetryWithoutMark(t *testing.T) {
	t.Parallel()
	policy := recoveryTestPolicy()
	policy.RetryActions = true
	ctx, cancel := NewContext(allocCtx, WithRecovery(policy))
	defer cancel()

	if err := Do(ctx, Navigate(testdataDir+"/console.html")); err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int32
	a := Func(func(ctx context.Context, t *Target) error {
		runs.Add(1)
		_, err := cdp.Call(ctx, t, page.Crash, cdp.Empty{})
		return err
	})
	err := Do(ctx, a)
	if !errors.Is(err, ErrTargetCrashed) {
		t.Fatalf("want the crash error, got %v", err)
	}
	if runs.Load() != 1 {
		t.Fatalf("want 1 run of the action, got %d", runs.Load())
	}
}

// TestRecoveryTargetLimit makes sure that the policy stops after its limit of
// target attempts and returns ErrRecoveryLimit that wraps the last error.
func TestRecoveryTargetLimit(t *testing.T) {
	t.Parallel()
	policy := recoveryTestPolicy()
	policy.MaxTargetAttempts = 2
	policy.RetryActions = true
	ctx, cancel := NewContext(allocCtx, WithRecovery(policy))
	defer cancel()

	if err := Do(ctx, Navigate(testdataDir+"/console.html")); err != nil {
		t.Fatal(err)
	}

	// Every run crashes the target that it gets. The policy rebuilds twice and
	// then reaches the limit.
	alwaysCrash := Idempotent(Func(func(ctx context.Context, t *Target) error {
		_, _ = cdp.Call(ctx, t, page.Crash, cdp.Empty{})
		return ErrTargetCrashed
	}))
	err := Do(ctx, alwaysCrash)
	if !errors.Is(err, ErrRecoveryLimit) {
		t.Fatalf("want ErrRecoveryLimit, got %v", err)
	}
	if !errors.Is(err, ErrTargetCrashed) {
		t.Fatalf("the limit error must wrap the crash error, got %v", err)
	}
	state := FromContext(ctx).recovery
	if state.targetAttempts != 2 {
		t.Fatalf("want 2 target attempts, got %d", state.targetAttempts)
	}
}

// TestRecoveryConcurrentSingleFlight makes sure that two calls that see the
// same crashed target trigger one rebuild, not two. The package does not
// support concurrent calls on one context, so this calls the rebuild path in
// series and checks the generation.
func TestRecoveryConcurrentSingleFlight(t *testing.T) {
	t.Parallel()
	ctx, cancel := NewContext(allocCtx, WithRecovery(recoveryTestPolicy()))
	defer cancel()

	if err := Do(ctx, Navigate(testdataDir+"/console.html")); err != nil {
		t.Fatal(err)
	}
	c := FromContext(ctx)
	before := c.recovery.generation
	if err := Do(ctx, crashPage()); !errors.Is(err, ErrTargetCrashed) {
		t.Fatalf("want the crash error, got %v", err)
	}
	after := c.recovery.generation
	if after != before+1 {
		t.Fatalf("want generation %d, got %d", before+1, after)
	}

	// A second call on the new target must not count another attempt when
	// nothing failed.
	if err := Do(ctx, Idempotent(Func(func(ctx context.Context, t *Target) error {
		return nil
	}))); err != nil {
		t.Fatal(err)
	}
	if c.recovery.generation != after {
		t.Fatal("a successful action changed the generation")
	}
}

// TestRecoveryDefaultHasNoPolicy makes sure that a context without WithRecovery
// returns the crash error and keeps the dead target, so the default behavior of
// the package does not change.
func TestRecoveryDefaultHasNoPolicy(t *testing.T) {
	t.Parallel()
	ctx, cancel := NewContext(allocCtx)
	defer cancel()

	if err := Do(ctx, Navigate(testdataDir+"/console.html")); err != nil {
		t.Fatal(err)
	}
	dead := FromContext(ctx).Target
	err := Do(ctx, crashPage())
	if !errors.Is(err, ErrTargetCrashed) {
		t.Fatalf("want ErrTargetCrashed, got %v", err)
	}
	if FromContext(ctx).Target != dead {
		t.Fatal("a context without a policy must keep its target")
	}
	if FromContext(ctx).recovery != nil {
		t.Fatal("a context without WithRecovery has recovery state")
	}
}

// TestRecoveryCancelStopsRebuild makes sure that a canceled context ends the
// backoff and the rebuild at once, with the error of the context and no
// deadlock.
func TestRecoveryCancelStopsRebuild(t *testing.T) {
	t.Parallel()
	policy := recoveryTestPolicy()
	// A long backoff gives the cancel time to win.
	policy.InitialBackoff = 30 * time.Second
	policy.MaxBackoff = 30 * time.Second
	ctx, cancel := NewContext(allocCtx, WithRecovery(policy))
	defer cancel()

	if err := Do(ctx, Navigate(testdataDir+"/console.html")); err != nil {
		t.Fatal(err)
	}
	child, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	var got error
	go func() {
		defer wg.Done()
		got = Do(child, crashPage())
	}()
	// Let the action crash and enter the backoff, then cancel.
	time.Sleep(200 * time.Millisecond)
	stop()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the canceled recovery did not return")
	}
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", got)
	}
}

// closeOwnTarget closes the tab of the target through the browser. It models
// a navigation that interrupts the target, or a page that closes itself. The
// command on the target then ends with ErrTargetDetached.
func closeOwnTarget() Action[Void] {
	return Func(func(ctx context.Context, t *Target) error {
		_, err := cdp.Call(ctx, t.browser, target.CloseTarget,
			target.CloseTargetParams{TargetID: t.TargetID})
		return err
	})
}

// TestRecoveryDetachedTargetRebuilds makes sure that the policy rebuilds a
// target that the browser detached, and that the next action works.
func TestRecoveryDetachedTargetRebuilds(t *testing.T) {
	t.Parallel()
	ctx, cancel := NewContext(allocCtx, WithRecovery(recoveryTestPolicy()))
	defer cancel()

	pageURL := testdataDir + "/console.html"
	if err := Do(ctx, Navigate(pageURL)); err != nil {
		t.Fatal(err)
	}
	first := FromContext(ctx).Target

	// The action closes its own tab. Its follow-up commands end with
	// ErrTargetDetached, which the policy treats as a recoverable target loss.
	if err := Do(ctx, closeOwnTarget()); err != nil {
		// The browser can answer the CloseTarget call before the detach
		// reaches the target, so a nil error is allowed.
		t.Logf("closeOwnTarget returned %v", err)
	}

	// Give the detach event time to reach the target, then make a call that
	// triggers the rebuild. Sleep does not call the target, so use Title.
	waitDetached(t, ctx, first)

	second := FromContext(ctx).Target
	if second == first {
		if _, err := Run(ctx, Title()); err != nil && !errors.Is(err, ErrTargetDetached) {
			t.Fatalf("unexpected error after the detach: %v", err)
		}
		second = FromContext(ctx).Target
	}
	if second == first {
		t.Fatal("the context still holds the detached target")
	}
	if _, err := Run(ctx, Title()); err != nil {
		t.Fatalf("the action after the rebuild failed: %v", err)
	}
}

// waitDetached blocks until the target t reports its detach, or the test
// deadline ends.
func waitDetached(t *testing.T, ctx context.Context, tgt *Target) {
	t.Helper()
	select {
	case <-tgt.detached:
	case <-time.After(5 * time.Second):
		t.Fatal("the target did not report its detach")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// TestRecoveryRestoreFunc makes sure that the policy runs the Restore func on
// the new target. The func sets a marker in the page, and the next action
// reads it back.
func TestRecoveryRestoreFunc(t *testing.T) {
	t.Parallel()
	policy := recoveryTestPolicy()
	var restoreRuns atomic.Int32
	policy.Restore = Func(func(ctx context.Context, t *Target) error {
		restoreRuns.Add(1)
		_, err := cdp.Call(ctx, t, page.AddScriptToEvaluateOnNewDocument,
			page.AddScriptToEvaluateOnNewDocumentParams{Source: "window.__recovered = true"})
		return err
	})
	ctx, cancel := NewContext(allocCtx, WithRecovery(policy))
	defer cancel()

	if err := Do(ctx, Navigate(testdataDir+"/console.html")); err != nil {
		t.Fatal(err)
	}
	if err := Do(ctx, crashPage()); !errors.Is(err, ErrTargetCrashed) {
		t.Fatalf("want the crash error, got %v", err)
	}
	// Navigate again so the script of the Restore func runs on the new
	// document.
	if err := Do(ctx, Navigate(testdataDir+"/console.html")); err != nil {
		t.Fatal(err)
	}
	got, err := Run(ctx, Evaluate[bool]("window.__recovered === true"))
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("the Restore func did not run on the new target")
	}
	if restoreRuns.Load() != 1 {
		t.Fatalf("want 1 run of the Restore func, got %d", restoreRuns.Load())
	}
}

// TestRecoveryFrameInvalidated makes sure that nodes of the target that
// crashed are invalidated and the new target builds a fresh DOM tree. A node
// id of the old target must not resolve on the new one.
func TestRecoveryFrameInvalidated(t *testing.T) {
	t.Parallel()
	ctx, cancel := NewContext(allocCtx, WithRecovery(recoveryTestPolicy()))
	defer cancel()

	if err := Do(ctx, Navigate(testdataDir+"/console.html")); err != nil {
		t.Fatal(err)
	}
	old := FromContext(ctx).Target
	oldNodes, err := Run(ctx, Nodes(CSS("body")))
	if err != nil {
		t.Fatal(err)
	}
	if len(oldNodes) == 0 {
		t.Fatal("the test page has no body node")
	}
	oldNodeID := oldNodes[0].NodeID

	if err := Do(ctx, crashPage()); !errors.Is(err, ErrTargetCrashed) {
		t.Fatalf("want the crash error, got %v", err)
	}

	// The old frame marked its root invalid when the document changed. The
	// new target has its own frame map with its own root. The frame tree is
	// built from events after the rebuild, so wait until it is ready.
	newTarget := FromContext(ctx).Target
	if newTarget == old {
		t.Fatal("the context still holds the crashed target")
	}
	pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
	defer pcancel()
	if err := retryWithSleep(pctx, 50*time.Millisecond, func(context.Context) (bool, error) {
		newTarget.frameMu.RLock()
		ready := len(newTarget.frames) > 0
		newTarget.frameMu.RUnlock()
		return ready, nil
	}); err != nil {
		t.Fatal("the new target built no frame tree before the timeout")
	}

	// A DOM call on the new target works and returns its own document.
	doc, err := cdp.Call(ctx, newTarget, dom.GetDocument, dom.GetDocumentParams{})
	if err != nil {
		t.Fatalf("the new target could not read its document: %v", err)
	}
	if doc.Root == nil || doc.Root.NodeID == 0 {
		t.Fatal("the new target returned no document root")
	}
	if doc.Root.NodeID == oldNodeID {
		t.Fatal("the new target reused the node id of the crashed target")
	}
}

// TestRecoveryBrowserCrashRebuilds starts a browser that the context owns,
// kills its process, and makes sure that the policy allocates a new browser
// and a new target. It needs its own browser, so it does not use the shared
// browser of the other tests.
func TestRecoveryBrowserCrashRebuilds(t *testing.T) {
	t.Parallel()
	policy := recoveryTestPolicy()
	policy.MaxBrowserAttempts = 1
	policy.RetryActions = true
	ctx, cancel := NewContext(allocCtx, WithRecovery(policy))
	defer cancel()

	pageURL := testdataDir + "/console.html"
	if err := Do(ctx, Navigate(pageURL)); err != nil {
		t.Fatal(err)
	}
	c := FromContext(ctx)
	oldBrowser := c.Browser
	oldTarget := c.Target
	if oldBrowser.Process() == nil {
		t.Fatal("the test needs a browser that the allocator started")
	}

	// Kill the whole browser process. The next command loses the connection.
	if err := oldBrowser.Process().Signal(os.Kill); err != nil {
		t.Fatal(err)
	}

	// A read action marked idempotent retries after the new browser starts.
	// It polls the title, because the restored page can still load when the
	// retry starts.
	var title string
	recoverAction := Idempotent(Func(func(ctx context.Context, t *Target) error {
		return retryWithSleep(ctx, 50*time.Millisecond, func(ctx context.Context) (bool, error) {
			v, err := Title()(ctx, t)
			if err != nil {
				return false, err
			}
			if v == "" {
				return false, nil
			}
			title = v
			return true, nil
		})
	}))
	rctx, rc := context.WithTimeout(ctx, 45*time.Second)
	defer rc()
	if err := Do(rctx, recoverAction); err != nil {
		t.Fatalf("the action did not recover from the browser crash: %v", err)
	}
	if title == "" {
		t.Fatal("the new browser returned an empty title")
	}
	if FromContext(ctx).Browser == oldBrowser {
		t.Fatal("the context still holds the dead browser")
	}
	if FromContext(ctx).Target == oldTarget {
		t.Fatal("the context still holds the target of the dead browser")
	}

	// The browser that the recovery started must stop when the context ends.
	assertRecoveryBrowserStops(t, FromContext(ctx).Browser, cancel)
}
