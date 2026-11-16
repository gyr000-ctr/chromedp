package chromedp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
)

// maxSettleInflight is the largest number of requests that a network idle
// condition tracks at the same time. A page with more requests in flight
// still settles, because the tracker counts the rest as overflow and the
// quiet window works as before. Only the option WithNetworkInflight sees the
// overflow, as requests in flight.
const maxSettleInflight = 1024

// errConditionTimeout is the internal error of a condition whose own timer
// ended. The main loop of WaitSettled turns it into a SettleTimeoutError.
var errConditionTimeout = errors.New("condition timeout")

// settlePageTimerMargin is the time that the page-side timer of a DOM stable
// condition leaves before the deadline of the wait. The result of the
// condition, with the count of the mutations, then reaches Go before the
// context ends.
const settlePageTimerMargin = 200 * time.Millisecond

// settleTask holds the data of a WaitSettled action.
type settleTask struct {
	timeout       time.Duration
	maxInflight   int
	domAttributes bool
	network       bool
	conditions    []settleCondition
}

// settleCondition is one condition of a WaitSettled action. The run func
// returns nil when the condition holds. It returns errConditionTimeout when
// its own timer ended, and the error of ctx when the wait ended.
type settleCondition struct {
	name string
	run  func(ctx context.Context, t *Target, st *settleState) error
}

// settleState shares the data of one WaitSettled run between its conditions.
type settleState struct {
	network *networkTracker // nil when no condition watches the network

	mu        sync.Mutex
	mutations int
}

// addMutations adds n to the count of DOM mutations of the run.
func (st *settleState) addMutations(n int) {
	st.mu.Lock()
	st.mutations += n
	st.mu.Unlock()
}

// fillStats copies the counters of the run into stats.
func (st *settleState) fillStats(stats *SettleStats, start time.Time) {
	stats.Duration = time.Since(start)
	if tr := st.network; tr != nil {
		tr.mu.Lock()
		stats.Requests = tr.started
		stats.Finished = tr.finished
		stats.Failed = tr.failed
		stats.PeakInflight = tr.peak
		stats.WebSockets = tr.websockets
		tr.mu.Unlock()
	}
	st.mu.Lock()
	stats.Mutations = st.mutations
	st.mu.Unlock()
}

// SettleStats holds the statistics of one WaitSettled run.
type SettleStats struct {
	// Duration is the wall time of the whole wait.
	Duration time.Duration

	// Conditions holds one entry per condition, in the order of the options.
	Conditions []ConditionStats

	// Requests is the number of requests that started during the wait.
	// Finished and Failed count the requests that ended or failed.
	// PeakInflight is the largest number of requests in flight at one time.
	// WebSockets is the number of WebSockets that the page opened. All four
	// are zero when no network idle condition ran.
	Requests     int
	Finished     int
	Failed       int
	PeakInflight int
	WebSockets   int

	// Mutations is the number of DOM mutations that the DOM stable
	// conditions counted. It is zero when no DOM stable condition ran.
	Mutations int
}

// String returns a one-line summary of the statistics, for logs.
func (s SettleStats) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "waited %s", s.Duration)
	for _, c := range s.Conditions {
		if c.Met {
			fmt.Fprintf(&b, ", %s met after %s", c.Name, c.Duration)
		} else {
			fmt.Fprintf(&b, ", %s not met", c.Name)
		}
	}
	if s.Requests > 0 || s.PeakInflight > 0 {
		fmt.Fprintf(&b, ", %d requests (%d finished, %d failed, %d in flight at most)",
			s.Requests, s.Finished, s.Failed, s.PeakInflight)
	}
	if s.Mutations > 0 {
		fmt.Fprintf(&b, ", %d dom mutations", s.Mutations)
	}
	return b.String()
}

// ConditionStats holds the statistics of one condition of a WaitSettled run.
type ConditionStats struct {
	// Name names the condition, for example "network idle".
	Name string

	// Met is true when the condition held before the wait ended.
	Met bool

	// Duration is the time from the start of the wait until the condition
	// held. It is zero when the condition did not hold.
	Duration time.Duration
}

// WaitSettled is an action that waits until every condition of the options
// holds, and returns the statistics of the wait. It serves pages that load
// their content in steps, such as a single page application, where a fixed
// selector or a fixed sleep is not enough.
//
// The options add the conditions:
//
//   - WithNetworkIdle waits until the network is quiet.
//   - WithDOMStable waits until the DOM does not change.
//   - WithElementVisible waits until an element is visible.
//   - WithJSCondition waits until a JavaScript expression is truthy.
//
// The conditions run at the same time, and the wait ends when every
// condition holds. WithSettleTimeout sets the limit of the whole wait. It
// defaults to 30 seconds, and 0 disables it. When the timeout ends first,
// the action returns a [SettleTimeoutError] that names the conditions that
// did not hold. When the context ends, the action returns the error of the
// context. The wait never blocks for ever.
//
// The action adds one event listener to the target for the run, and the end
// of the run removes it. The tracker of the requests in flight has a bound
// of 1024 entries. The action runs on the target of the context and opens no
// session of its own.
//
// With no options, the action has no conditions and returns at once.
//
// For example:
//
//	stats, err := chromedp.Run(ctx, chromedp.WaitSettled(
//		chromedp.WithNetworkIdle(500*time.Millisecond),
//		chromedp.WithDOMStable(500*time.Millisecond),
//		chromedp.WithElementVisible(chromedp.CSS("#app")),
//		chromedp.WithJSCondition(`window.appReady === true`),
//	))
func WaitSettled(opts ...SettleOption) Action[SettleStats] {
	task := &settleTask{timeout: 30 * time.Second, maxInflight: -1}
	for _, o := range opts {
		o(task)
	}
	return func(ctx context.Context, t *Target) (SettleStats, error) {
		return task.run(ctx, t)
	}
}

// run executes the wait. It starts one goroutine per condition, and ends
// when every condition holds, when a condition fails, or when wctx ends.
func (task *settleTask) run(ctx context.Context, t *Target) (SettleStats, error) {
	start := time.Now()
	stats := SettleStats{Conditions: make([]ConditionStats, len(task.conditions))}
	for i, c := range task.conditions {
		stats.Conditions[i].Name = c.name
	}
	if len(task.conditions) == 0 {
		stats.Duration = time.Since(start)
		return stats, nil
	}

	wctx := ctx
	cancel := context.CancelFunc(func() {})
	if task.timeout > 0 {
		wctx, cancel = context.WithTimeout(ctx, task.timeout)
	} else {
		wctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	st := &settleState{}
	if task.network {
		st.network = newNetworkTracker()
		// The listener stops when wctx ends, and the target drops it on
		// the next event.
		t.listen(wctx, st.network.handle)
	}

	type result struct {
		index int
		err   error
	}
	// The channel has room for every condition, so a condition that ends
	// after the wait never blocks.
	results := make(chan result, len(task.conditions))
	for i, c := range task.conditions {
		go func() {
			err := c.run(wctx, t, st)
			switch {
			case err == nil:
				results <- result{i, nil}
			case errors.Is(err, errConditionTimeout):
				// The timer of the condition ended at the deadline of
				// the wait. The wctx.Done case reports the timeout.
			case wctx.Err() != nil:
				// The wait ended while the condition ran.
			default:
				results <- result{i, err}
			}
		}()
	}

	pending := len(task.conditions)
	for pending > 0 {
		select {
		case r := <-results:
			if r.err != nil {
				cancel()
				st.fillStats(&stats, start)
				return stats, fmt.Errorf("waiting for %s: %w", task.conditions[r.index].name, r.err)
			}
			pending--
			cs := &stats.Conditions[r.index]
			cs.Met = true
			cs.Duration = time.Since(start)
		case <-wctx.Done():
			st.fillStats(&stats, start)
			if ctx.Err() != nil {
				// The caller ended the context. This is not a timeout
				// of the wait itself.
				return stats, ctx.Err()
			}
			return stats, task.timeoutError(&stats)
		}
	}
	st.fillStats(&stats, start)
	return stats, nil
}

// timeoutError builds the error of a wait whose timeout ended. It names the
// conditions that did not hold.
func (task *settleTask) timeoutError(stats *SettleStats) error {
	var pending []string
	for _, cs := range stats.Conditions {
		if !cs.Met {
			pending = append(pending, cs.Name)
		}
	}
	return &SettleTimeoutError{Pending: pending, Stats: *stats}
}

// SettleOption is a WaitSettled option.
type SettleOption = func(*settleTask)

// WithSettleTimeout sets the limit of the whole wait. It defaults to 30
// seconds. Pass 0 to disable it, and then only the end of the context stops
// the wait.
func WithSettleTimeout(timeout time.Duration) SettleOption {
	return func(task *settleTask) {
		task.timeout = timeout
	}
}

// WithNetworkIdle adds a network idle condition with the given quiet window.
// The network is idle when no request started, received data, ended or
// failed during the window. Only activity resets the window, so a request
// that stays open does not stop the condition. This covers a long poll and
// the handshake of a WebSocket. A page that sends requests all the time
// never turns idle, and the wait ends with a [SettleTimeoutError].
func WithNetworkIdle(quiet time.Duration) SettleOption {
	return func(task *settleTask) {
		task.network = true
		task.conditions = append(task.conditions, settleCondition{
			name: "network idle",
			run: func(ctx context.Context, t *Target, st *settleState) error {
				tr := st.network
				interval := max(quiet/4, 10*time.Millisecond)
				for {
					tr.mu.Lock()
					idle := time.Since(tr.lastActivity) >= quiet &&
						(task.maxInflight < 0 || len(tr.inflight)+tr.overflow <= task.maxInflight)
					tr.mu.Unlock()
					if idle {
						return nil
					}
					if err := sleepContext(ctx, interval); err != nil {
						return err
					}
				}
			},
		})
	}
}

// WithNetworkInflight makes the network idle condition also require that at
// most n requests are in flight. Without this option, the condition looks at
// the activity only, and a request that stays open does not stop it. The
// option has no effect without WithNetworkIdle.
func WithNetworkInflight(n int) SettleOption {
	return func(task *settleTask) {
		task.maxInflight = n
	}
}

// WithDOMStable adds a DOM stable condition with the given quiet window. The
// DOM is stable when no mutation of the tree happened during the window. A
// mutation is an added or removed node or a change of text. Mutations of
// attributes do not count by default, so that an animation that changes a
// style or a class does not stop the condition. WithDOMStableAttributes
// makes them count.
func WithDOMStable(quiet time.Duration) SettleOption {
	return func(task *settleTask) {
		task.conditions = append(task.conditions, settleCondition{
			name: "dom stable",
			run: func(ctx context.Context, t *Target, st *settleState) error {
				var execCtx runtime.ExecutionContextID
				for {
					var ok bool
					_, _, execCtx, ok = t.ensureFrame()
					if ok {
						break
					}
					if err := sleepContext(ctx, 5*time.Millisecond); err != nil {
						return err
					}
				}
				var timeoutMs int64
				if deadline, ok := ctx.Deadline(); ok {
					// The page-side timer ends the wait in the page a
					// little before the deadline, so that the result
					// reaches Go before the context ends, and so that
					// a timed out wait leaves no observer behind.
					timeoutMs = max(time.Until(deadline).Milliseconds()-settlePageTimerMargin.Milliseconds(), 1)
				}
				res, _, err := callFunctionOn[domStableResult](ctx, t, domStableJS,
					func(p *runtime.CallFunctionOnParams) {
						p.ExecutionContextID = execCtx
						p.AwaitPromise = ptr(true)
					},
					quiet.Milliseconds(), timeoutMs, task.domAttributes,
				)
				if err != nil {
					return err
				}
				st.addMutations(res.Mutations)
				if !res.Stable {
					return errConditionTimeout
				}
				return nil
			},
		})
	}
}

// WithDOMStableAttributes makes the DOM stable condition count mutations of
// attributes too. Use it when the change that you wait for sets an
// attribute, such as a class. An animation that changes a style or a class
// then stops the condition from settling. The option has no effect without
// WithDOMStable.
func WithDOMStableAttributes() SettleOption {
	return func(task *settleTask) {
		task.domAttributes = true
	}
}

// WithElementVisible adds a condition that waits until an element that
// matches the selector is visible, as [WaitVisible] does. The query options
// change the lookup as in a query action.
func WithElementVisible[S Selectable](sel S, opts ...QueryOption) SettleOption {
	return func(task *settleTask) {
		task.conditions = append(task.conditions, settleCondition{
			name: fmt.Sprintf("element %v visible", sel),
			run: func(ctx context.Context, t *Target, st *settleState) error {
				_, err := WaitVisible(sel, opts...)(ctx, t)
				return err
			},
		})
	}
}

// WithJSCondition adds a condition that waits until the JavaScript
// expression returns a truthy value, as [Poll] does. The condition polls
// the expression every 100 milliseconds, so it also works on a tab that gets
// no animation frames.
func WithJSCondition(expression string) SettleOption {
	return func(task *settleTask) {
		task.conditions = append(task.conditions, settleCondition{
			name: fmt.Sprintf("js %s", expression),
			run: func(ctx context.Context, t *Target, st *settleState) error {
				p := &pollTask{
					predicate: fmt.Sprintf(`return (%s);`, expression),
					interval:  100 * time.Millisecond,
				}
				if deadline, ok := ctx.Deadline(); ok {
					p.timeout = time.Until(deadline)
				}
				_, err := runPoll[Void](ctx, t, p)
				if errors.Is(err, ErrPollingTimeout) {
					return errConditionTimeout
				}
				return err
			},
		})
	}
}

// domStableResult is the result of the domStableJS function.
type domStableResult struct {
	Stable    bool `json:"stable"`
	Mutations int  `json:"mutations"`
}

// networkTracker counts the requests of a target during one WaitSettled
// run. The map of the requests in flight has the bound maxSettleInflight,
// and the rest counts as overflow.
type networkTracker struct {
	mu           sync.Mutex
	inflight     map[network.RequestID]struct{}
	overflow     int // requests in flight beyond maxSettleInflight
	lastActivity time.Time
	started      int
	finished     int
	failed       int
	peak         int
	websockets   int
}

func newNetworkTracker() *networkTracker {
	return &networkTracker{
		inflight: make(map[network.RequestID]struct{}),
		// The quiet window starts with the wait, so a request that ended
		// before the wait does not count.
		lastActivity: time.Now(),
	}
}

// handle records a network event. It runs on the goroutine that reads the
// messages of the tab, so it must be fast and must not block.
func (tr *networkTracker) handle(ev any) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	switch ev := ev.(type) {
	case *network.EventRequestWillBeSent:
		tr.lastActivity = time.Now()
		if _, ok := tr.inflight[ev.RequestID]; ok {
			// A redirect sends the event again with the same id.
			return
		}
		tr.started++
		if len(tr.inflight) < maxSettleInflight {
			tr.inflight[ev.RequestID] = struct{}{}
		} else {
			tr.overflow++
		}
		if n := len(tr.inflight) + tr.overflow; n > tr.peak {
			tr.peak = n
		}
	case *network.EventDataReceived:
		tr.lastActivity = time.Now()
	case *network.EventLoadingFinished:
		tr.finished++
		tr.lastActivity = time.Now()
		tr.remove(ev.RequestID)
	case *network.EventLoadingFailed:
		tr.failed++
		tr.lastActivity = time.Now()
		tr.remove(ev.RequestID)
	case *network.EventWebSocketCreated:
		tr.websockets++
	}
}

// remove drops a request from the in-flight count.
func (tr *networkTracker) remove(id network.RequestID) {
	if _, ok := tr.inflight[id]; ok {
		delete(tr.inflight, id)
	} else if tr.overflow > 0 {
		tr.overflow--
	}
}
