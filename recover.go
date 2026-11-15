package chromedp

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
)

// Default limits of a [RecoveryPolicy].
const (
	// defaultRecoveryAttempts is the number of target rebuilds after the
	// first failure. Zero turns recovery off.
	defaultRecoveryAttempts = 3

	// defaultBrowserAttempts is the number of browser allocations after the
	// first browser stops.
	defaultBrowserAttempts = 2

	// defaultRecoveryBackoff is the wait before the first attempt.
	defaultRecoveryBackoff = 200 * time.Millisecond

	// maxRecoveryBackoff is the longest wait between two attempts.
	maxRecoveryBackoff = 10 * time.Second

	// defaultRecoveryTimeout bounds one rebuild of a browser or a target.
	defaultRecoveryTimeout = 30 * time.Second
)

// RecoveryPolicy tells a chromedp context what to do when the renderer process
// of its target stops, or when the whole browser stops. A context without a
// policy keeps the behavior of earlier versions: a failed action returns an
// error and nothing is rebuilt. See [WithRecovery].
//
// The zero value is not a valid policy. Use [DefaultRecoveryPolicy] as the
// base and change the fields you need.
type RecoveryPolicy struct {
	// MaxTargetAttempts bounds the number of times the policy rebuilds the
	// target of the context after its renderer process stops. Zero turns
	// target recovery off.
	MaxTargetAttempts int

	// MaxBrowserAttempts bounds the number of times the policy allocates a
	// new browser after the browser process stops. It applies only when the
	// allocator starts a browser. An allocator that attaches to a browser
	// that runs already cannot restart it, so the policy then returns
	// [ErrBrowserDied]. Zero turns browser recovery off.
	MaxBrowserAttempts int

	// InitialBackoff is the wait before the first attempt. Each later wait
	// doubles, with jitter, up to MaxBackoff.
	InitialBackoff time.Duration

	// MaxBackoff bounds the wait between two attempts.
	MaxBackoff time.Duration

	// Timeout bounds one rebuild. It covers the allocation of a browser and
	// the first navigation of the new target. Zero means no bound. A canceled
	// parent context always ends the wait at once.
	Timeout time.Duration

	// RestoreURL makes the policy navigate the new target to the URL that the
	// target had loaded before it stopped. The URL comes from the frame tree
	// of the target.
	RestoreURL bool

	// Restore runs on the new target after the target is rebuilt and, when
	// RestoreURL is true, after the navigation. Use it to put back the state
	// that a new tab does not keep, such as a login, storage values or headers.
	// It must be safe to call again, because every attempt calls it. It runs
	// as an action on the new target. A nil func does nothing.
	Restore Action[Void]

	// RetryActions makes [Run] run the action of the call again after a
	// successful rebuild. The action must be idempotent, which means safe to
	// run twice. Wrap the action with [Idempotent] to mark it. When this field
	// is false, Run returns the error of the crash and the caller decides what
	// to do next. The package never runs an action twice unless both this
	// field and the [Idempotent] mark ask for it.
	RetryActions bool
}

// DefaultRecoveryPolicy returns a policy with safe limits. It rebuilds the
// target up to three times and the browser up to two times. It restores the
// URL. It does not run the action again, because the package cannot know
// whether an action has side effects. Set RetryActions to true to opt in.
func DefaultRecoveryPolicy() RecoveryPolicy {
	return RecoveryPolicy{
		MaxTargetAttempts:  defaultRecoveryAttempts,
		MaxBrowserAttempts: defaultBrowserAttempts,
		InitialBackoff:     defaultRecoveryBackoff,
		MaxBackoff:         maxRecoveryBackoff,
		Timeout:            defaultRecoveryTimeout,
		RestoreURL:         true,
	}
}

// WithRecovery gives the context a policy that rebuilds its target or its
// browser when they stop. Without this option, the context does no recovery.
// A child context does not inherit the policy. Give the option to each context
// that needs recovery.
//
// The option changes only what happens after a crash. A context without it
// behaves exactly as before.
func WithRecovery(p RecoveryPolicy) ContextOption {
	return func(c *Context) {
		policy := p
		c.recovery = &recoveryState{policy: policy}
	}
}

// Idempotent marks an action as safe to run again after a recovery. Use it
// together with a policy that sets RetryActions. The wrapper changes nothing
// about the action itself. The wrapper sets a flag on the recovery state of
// the context while the wrapped action runs, so [Run] can check it after a
// failure.
//
// An action is idempotent when running it twice gives the same result as
// running it once. A read is idempotent. A click that sends a form is not.
func Idempotent(a Action[Void]) Action[Void] {
	return func(ctx context.Context, t *Target) (Void, error) {
		if c := FromContext(ctx); c != nil && c.recovery != nil {
			c.recovery.idempotent = true
		}
		return a(ctx, t)
	}
}

// recoveryState holds the counters and the locks of one context. A context
// creates it once, when [WithRecovery] gives it a policy. The fields use fixed
// size types only, so repeated recovery grows no slice and no map.
type recoveryState struct {
	policy RecoveryPolicy

	// rebuildMu makes the rebuild single-flight. Two calls of Run that see
	// the same crash wait for one rebuild instead of starting two tabs.
	rebuildMu sync.Mutex

	// generation increases each time the context gets a new target. The first
	// target has generation zero. It lets a call of Run learn that another
	// call rebuilt the target already.
	generation uint64

	// targetAttempts and browserAttempts count the attempts of the policy.
	// The counters never reset, so a target that crashes for ever reaches the
	// limit instead of recovering for ever.
	targetAttempts  int
	browserAttempts int

	// idempotent is true while the action that runs now carries the mark that
	// [Idempotent] sets. withRecovery resets it before each call and reads it
	// after a failure. Actions on one context run in series, as the docs of
	// NewContext require, so the flag needs no lock.
	idempotent bool

	// lastURL is the URL of the top frame of the current target. The listener
	// of the context writes it, and the rebuild reads it.
	lastURLMu sync.RWMutex
	lastURL   string
}

// withRecovery runs a with the recovery policy of c, when c has one. Without
// a policy it runs a once and returns its result, as [Run] did before.
func withRecovery[T any](ctx context.Context, c *Context, a Action[T]) (T, error) {
	var zero T
	state := c.recovery
	if state == nil {
		return a(ctx, c.Target)
	}
	// recCtx survives the cancel of a dead browser, but ends when the caller
	// ends the call context or the context of NewContext. The rebuild and its
	// backoff use it.
	recCtx, stopRec := c.newRecoveryContext(ctx)
	defer stopRec()
	runCtx := ctx

	for {
		generation := state.generation
		// Reset the mark before the action runs. The Idempotent wrapper sets
		// it while the action runs.
		state.idempotent = false
		v, err := a(runCtx, c.Target)
		retry := state.policy.RetryActions && state.idempotent
		state.idempotent = false
		if err == nil {
			return v, nil
		}
		kind := classifyFailure(c, err)
		if kind == failureNone {
			return zero, err
		}
		// The caller ended the call or the context of NewContext. No recovery
		// can continue.
		if recCtx.Err() != nil {
			return zero, err
		}

		rerr := c.recoverAfter(recCtx, kind, generation, err)
		if rerr != nil {
			return zero, rerr
		}
		if !retry {
			// The new target is ready for the next call of Run. This call
			// returns the error of the crash, because the package must not run
			// an action with possible side effects again.
			return zero, err
		}
		// Run the action again on the recovery context and the new target. The
		// call context can be canceled from the dead browser, so it cannot
		// serve a retry. The attempt counters stop a loop that never succeeds.
		runCtx = recCtx
	}
}

// browserLost reports whether the connection to the browser is closed.
func (c *Context) browserLost() bool {
	if c.Browser == nil {
		return false
	}
	select {
	case <-c.Browser.LostConnection:
		return true
	default:
		return false
	}
}

// failureKind names the kind of a failure that a policy can recover from.
type failureKind int

const (
	failureNone failureKind = iota
	failureTarget
	failureBrowser
)

// classifyFailure tells whether err is a stopped target or a stopped browser.
// A canceled context and every other error return failureNone.
func classifyFailure(c *Context, err error) failureKind {
	var crash *TargetCrashError
	switch {
	case errors.As(err, &crash):
		return failureTarget
	case errors.Is(err, ErrTargetCrashed):
		return failureTarget
	case errors.Is(err, ErrTargetDetached):
		// The browser closed the tab. A policy can open a new one.
		return failureTarget
	case errors.Is(err, ErrBrowserDied):
		return failureBrowser
	}
	if c.Browser != nil {
		select {
		case <-c.Browser.LostConnection:
			return failureBrowser
		default:
		}
	}
	return failureNone
}

// recoverAfter rebuilds the target or the browser after a failure. It is
// single-flight: a call that sees a newer generation returns at once, because
// another call did the rebuild. It returns [ErrRecoveryLimit] when the policy
// reaches its limit. cause is the error of the action that failed, and the
// limit error wraps it.
func (c *Context) recoverAfter(ctx context.Context, kind failureKind, generation uint64, cause error) error {
	state := c.recovery
	if state == nil {
		return nil
	}
	state.rebuildMu.Lock()
	defer state.rebuildMu.Unlock()

	if state.generation != generation && kind == failureTarget {
		// Another call rebuilt the target while this call waited on the lock.
		return nil
	}

	switch kind {
	case failureTarget:
		if state.targetAttempts >= state.policy.MaxTargetAttempts {
			return limitError(failureTarget, state, cause)
		}
		state.targetAttempts++
	case failureBrowser:
		if state.browserAttempts >= state.policy.MaxBrowserAttempts {
			return limitError(failureBrowser, state, cause)
		}
		state.browserAttempts++
	default:
		return nil
	}

	if err := state.waitBackoff(ctx); err != nil {
		return err
	}

	rctx, cancel := state.rebuildContext(ctx)
	defer cancel()

	if kind == failureBrowser {
		if err := c.rebuildBrowser(rctx); err != nil {
			return limitError(failureBrowser, state, joinCause(cause, err))
		}
	}
	if err := c.rebuildTarget(rctx); err != nil {
		return limitError(kind, state, joinCause(cause, err))
	}
	return nil
}

// joinCause combines the error of the failed action with the error of the
// rebuild. The chain keeps both, so errors.Is and errors.As find both.
func joinCause(cause, rebuild error) error {
	return fmt.Errorf("%w: %w", cause, rebuild)
}

// waitBackoff waits the backoff of the next attempt, with jitter to spread
// attempts of several contexts. A canceled context ends the wait at once.
func (s *recoveryState) waitBackoff(ctx context.Context) error {
	attempt := s.targetAttempts + s.browserAttempts
	base := s.policy.InitialBackoff
	if base <= 0 {
		return nil
	}
	max := s.policy.MaxBackoff
	if max <= 0 {
		max = maxRecoveryBackoff
	}
	d := base
	for range attempt - 1 {
		next := d * 2
		if next > max || next <= 0 {
			d = max
			break
		}
		d = next
	}
	// Jitter of up to 25 percent keeps two contexts from retrying together.
	jitter := time.Duration(rand.Int64N(int64(d)/4 + 1))
	return sleepContext(ctx, d+jitter)
}

// rebuildContext returns a context for one rebuild. It carries the timeout of
// the policy and ends when the parent context ends.
func (s *recoveryState) rebuildContext(parent context.Context) (context.Context, context.CancelFunc) {
	if s.policy.Timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, s.policy.Timeout)
}

// newRecoveryContext returns a context that the recovery uses. It derives from
// durable, so the cancel of a dead browser does not reach it directly. It ends
// when the caller ends the call while the browser still works, or when the
// caller ends the context of NewContext. When the browser died, the cancel of
// the call is the internal cancel of the allocator, so the context stays open
// and the recovery can rebuild the browser. The returned cancel func frees the
// timers and must run when the rebuild is done.
func (c *Context) newRecoveryContext(call context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(c.durableCtx)
	stopDurable := context.AfterFunc(c.durableCtx, cancel)
	stopCall := context.AfterFunc(call, func() {
		// The allocator cancels the call when the browser dies. That cancel
		// must not stop the recovery. A cancel while the browser still works
		// comes from the caller and must stop it.
		if !c.browserLost() {
			cancel()
		}
	})
	return ctx, func() {
		stopCall()
		stopDurable()
		cancel()
	}
}

// rebuildBrowser stops the old browser and allocates a new one with the same
// allocator and the same options. It works only for the context that owns the
// browser and for an allocator that starts a browser. A child context shares
// the browser with its parent, so it cannot replace the browser without
// breaking the other contexts. It returns [ErrBrowserDied] then. The new
// browser uses a context of its own, because the context of the old browser
// ended when the browser stopped.
func (c *Context) rebuildBrowser(ctx context.Context) error {
	if !c.first || isAttacher(c.Allocator) {
		return ErrBrowserDied
	}
	if c.Browser != nil {
		select {
		case <-c.Browser.LostConnection:
		default:
			// The connection still works. Close the browser before a new one
			// takes its user data directory.
			c.Browser.markClosingGracefully()
			_ = c.Browser.conn.Close()
		}
	}
	if c.stopRecoveryBrowser != nil {
		// End the allocation context of the browser of an earlier rebuild.
		c.stopRecoveryBrowserOnce()
	}

	// The allocator reads its chromedp Context from the context. Allocate on
	// a fresh chromedp context that shares the allocator and the options.
	allocCtx, stopAlloc := context.WithCancel(context.Background())
	ac := &Context{
		Allocator: c.Allocator,
		allocated: make(chan struct{}, 1),
		cancel:    stopAlloc,
	}
	ac.allocated <- struct{}{}
	allocCtx = context.WithValue(allocCtx, contextKey{}, ac)

	b, err := c.Allocator.Allocate(allocCtx, c.browserOpts...)
	if err != nil {
		stopAlloc()
		return fmt.Errorf("allocating a new browser: %w", err)
	}
	c.Browser = b
	c.setRecoveryBrowser(stopAlloc)
	return nil
}

// rebuildTarget closes what remains of the old target, opens a new target on
// the current browser, restores the URL and runs the Restore func. A renderer
// crash leaves a target that cannot serve a new session, and a detached target
// is gone, so the policy opens a new tab in both cases. It then publishes the
// new target under the lock of the context.
func (c *Context) rebuildTarget(ctx context.Context) error {
	old := c.Target
	if old != nil {
		old.closeForRecovery(ctx)
	}

	c.targetMu.Lock()
	// newTarget waits for the first tab of a new browser when targetID is
	// empty and the context is first. That tab already exists, so the wait
	// never ends. Clear both, so newTarget opens a tab with CreateTarget.
	c.targetID = ""
	wasFirst := c.first
	c.first = false
	c.targetMu.Unlock()

	if err := c.newTarget(ctx); err != nil {
		// Restore the flag so the error path keeps the state of the context.
		c.targetMu.Lock()
		c.first = wasFirst
		c.targetMu.Unlock()
		return fmt.Errorf("opening a new target: %w", err)
	}
	c.recovery.generation++

	if c.recovery.policy.RestoreURL {
		url := c.recovery.url()
		if url != "" && url != "about:blank" {
			if _, err := cdp.Call(ctx, c.Target, page.Navigate, page.NavigateParams{URL: url}); err != nil {
				return fmt.Errorf("restoring the URL %q: %w", url, err)
			}
		}
	}
	if c.recovery.policy.Restore != nil {
		if _, err := c.recovery.policy.Restore(ctx, c.Target); err != nil {
			return fmt.Errorf("running the restore func: %w", err)
		}
	}
	return nil
}

// closeForRecovery ends a target that the policy replaces. It first asks the
// browser to detach the session and close the tab, so that the browser stops
// routing messages to the target. Only then it stops the goroutines of the
// target. The order matters. Stopping the goroutines first makes the browser
// block on the full queue of the target, and every later command hangs.
//
// The browser can be gone already, for example after a browser rebuild. The
// method then skips the browser commands and frees only the local resources.
func (t *Target) closeForRecovery(ctx context.Context) {
	if t.browser != nil {
		select {
		case <-t.browser.LostConnection:
		default:
			if t.SessionID != "" {
				_, _ = cdp.Call(ctx, t.browser, target.DetachFromTarget,
					target.DetachFromTargetParams{SessionID: t.SessionID})
			}
			if t.TargetID != "" {
				_, _ = cdp.Call(ctx, t.browser, target.CloseTarget,
					target.CloseTargetParams{TargetID: t.TargetID})
			}
		}
	}
	if t.stopRun != nil {
		t.stopRun()
	}
	t.events.close()
}

// attachRecoveryListener keeps the URL of the top frame of t up to date. The
// target calls the func on the goroutine that reads its messages, so the func
// does no command and does no block. It stores one string and grows no queue.
func (c *Context) attachRecoveryListener(t *Target, ctx context.Context) {
	t.listen(ctx, func(ev any) {
		e, ok := ev.(*page.EventFrameNavigated)
		if !ok || e.Frame.ParentID != "" || e.Frame.URL == "" {
			return
		}
		c.recovery.recordURL(e.Frame.URL)
	})
}

// recordURL stores url as the last URL of the target of the context.
func (s *recoveryState) recordURL(url string) {
	s.lastURLMu.Lock()
	s.lastURL = url
	s.lastURLMu.Unlock()
}

// url returns the last URL that recordURL stored.
func (s *recoveryState) url() string {
	s.lastURLMu.RLock()
	defer s.lastURLMu.RUnlock()
	return s.lastURL
}

// limitError builds the error for a policy that reached its limit. It wraps
// the error of the failed action and the error of the last rebuild.
func limitError(kind failureKind, s *recoveryState, cause error) error {
	var err error
	switch kind {
	case failureBrowser:
		err = fmt.Errorf("%w: browser attempts %d", ErrRecoveryLimit, s.browserAttempts)
	default:
		err = fmt.Errorf("%w: target attempts %d", ErrRecoveryLimit, s.targetAttempts)
	}
	if cause != nil {
		err = fmt.Errorf("%w: %w", err, cause)
	}
	return err
}
