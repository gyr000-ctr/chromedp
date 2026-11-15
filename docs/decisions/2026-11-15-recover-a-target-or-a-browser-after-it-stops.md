# Recover a target or a browser after it stops

Status: Decided.

## Context

A tab can lose its renderer process. The causes include an out of memory
kill, a renderer crash, and a navigation that stops the target. The browser
process itself can stop too. Earlier versions of chromedp left the context in
a broken state. A command then hung or returned an error that was hard to
read. The caller had to build all recovery code on its own.

## What was done

A new option, `WithRecovery`, gives a context a `RecoveryPolicy`. A context
without the option keeps the old behavior. A failed action returns an error
and the package rebuilds nothing. This keeps the change opt-in.

The policy can do these things:

1. Detect the stop. The target now handles the events
   `Inspector.targetCrashed` and `Target.targetCrashed`. A command on the
   target returns a `TargetCrashError` that wraps the sentinel
   `ErrTargetCrashed`. The error holds the status and the error code of the
   browser. A lost browser connection wraps the sentinel `ErrBrowserDied`.
2. Rebuild the target. The policy first detaches the old session and closes
   the old tab, so the browser stops routing messages to it. Then it opens a
   new tab. It can navigate to the last URL and run a caller supplied
   `Restore` action.
3. Rebuild the browser. When the context owns the browser and the allocator
   starts processes, the policy allocates a new browser with the same
   allocator and the same options. It then rebuilds the target.
4. Bound the work. The policy limits the number of target attempts and
   browser attempts. A backoff with jitter separates the attempts. The
   counters never reset, so a target that stops for ever reaches the limit
   instead of retrying for ever.
5. Stop on cancel. A caller that cancels the context ends the backoff and
   the rebuild at once.

## How side effects stay safe

The package does not know whether an action has side effects. By default it
runs an action once. After a rebuild, the failed call returns the error of
the crash, and the next call works on the new target.

A caller marks a read only action with `Idempotent`. When the policy also
sets `RetryActions`, the package runs the marked action again on the new
target. An action without the mark never runs twice. This prevents a
duplicate form post or a duplicate purchase.

## How a browser death does not cancel the recovery

The allocator cancels the context of a browser when the browser dies. That
cancel also cancels a rebuild. Each context now holds a durable context.
It derives from the context that the caller passed to `NewContext`, which the
allocator never cancels. The rebuild and the rebuilt target run on the
durable context. A recovery context combines it with the context of one call,
so a caller that cancels a call still stops the recovery while a dead browser
does not.

## How the memory stays bounded

The recovery state holds fixed size counters and one string for the last URL.
It keeps no queue of events and no list of past actions. The listener that
tracks the URL stores one string. Old targets close their subscriptions when
the policy replaces them, so repeated recovery grows no memory per step.

## Errors

The new sentinel errors are `ErrTargetCrashed`, `ErrBrowserDied` and
`ErrRecoveryLimit`. A limit error wraps the error of the failed action and
the error of the last rebuild, so `errors.Is` and `errors.As` find the cause.
A failure returns an error instead of hanging.

## What was not done

- Automatic replay of actions with side effects. The caller must opt in with
  `Idempotent`, because the package cannot prove that an action is safe to
  run twice.
- Recovery for an allocator that attaches to a browser that runs already.
  Such an allocator cannot restart the browser, so the policy returns
  `ErrBrowserDied` when the browser stops.
