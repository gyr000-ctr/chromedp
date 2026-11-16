# Wait for a settled page with WaitSettled

Status: Proposed.

A scraper for a single page application cannot rely on a fixed selector or a
fixed sleep. The page loads its content in steps: the first requests, the
skeleton screen, the lazy content. This change adds `WaitSettled`, an action
that waits until a set of conditions holds at the same time. The status is
Proposed until the maintainer reviews it.

## Why

The existing waits look at one thing. `WaitVisible` waits for an element,
and `Poll` waits for a JavaScript predicate. Neither one knows the network.
A page with a long poll or a WebSocket never reaches zero requests in
flight, so a naive network idle wait hangs for ever. A page with a request
storm never turns quiet either, and the wait must end with an error that
names the cause, not with a hang.

## What was done

- `WaitSettled(opts ...SettleOption) Action[SettleStats]` runs the
  conditions at the same time and ends when every condition holds.
- `WithNetworkIdle(quiet)` waits until no request started, received data,
  ended or failed during the quiet window. Only activity resets the window,
  so a request that stays open does not stop the wait. This covers a long
  poll and the handshake of a WebSocket. `WithNetworkInflight(n)` also
  limits the requests in flight.
- `WithDOMStable(quiet)` waits until the DOM does not change during the
  quiet window. A mutation is an added or removed node or a change of text.
  Mutations of attributes do not count by default, so an animation that
  changes a style or a class does not stop the wait.
  `WithDOMStableAttributes` counts them.
- `WithElementVisible(sel, opts...)` waits for a visible element, as
  `WaitVisible` does. `WithJSCondition(expression)` waits for a truthy
  JavaScript expression, as `Poll` does, with an interval of 100
  milliseconds.
- `WithSettleTimeout(d)` limits the whole wait. It defaults to 30 seconds,
  and 0 disables it. A timeout returns a `SettleTimeoutError`, which wraps
  `ErrSettleTimeout` and names the conditions that did not hold. A
  cancellation of the context returns the error of the context.
- The action returns `SettleStats`: the wall time, one entry per condition,
  the request counters and the DOM mutation count.
- The action adds one event listener to the target, and the end of the wait
  removes it. The map of the requests in flight has a bound of 1024 entries.
  The action opens no session of its own.
- The existing waits do not change.

## What was not done

- A condition for the frames of a WebSocket. A busy WebSocket is activity of
  the application, not of the network loader, and the wait ignores it.
- A way to wait for any one condition instead of all of them. `Poll` and the
  query waits cover the simple cases.
