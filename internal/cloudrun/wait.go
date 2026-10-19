package cloudrun

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Values a condition's Status takes. Unknown means it has not converged yet.
const (
	conditionTrue  = "True"
	conditionFalse = "False"
)

// Default values for the wait parameters. There is no default timeout here: a zero Timeout means
// no limit, for every wait, and the CLI's own default (10 minutes for a rollout or a deletion) is
// the flag's.
const (
	defaultWaitInterval = 2 * time.Second
	// Upper bound on the polling interval. The interval grows gradually and then levels off, so
	// the API is not hit continuously while waiting out a long startup.
	maxWaitInterval = 15 * time.Second
	waitBackoffNum  = 3
	waitBackoffDen  = 2
)

// WaitOptions controls how Wait behaves. The zero value is usable (defaults are filled in).
type WaitOptions struct {
	// Timeout is the upper bound on the whole wait. 0 means no limit, as in every wait clrnd has:
	// it used to mean 10 minutes here and no limit for an execution, so the same --timeout 0 meant
	// two different things depending on the command.
	Timeout time.Duration
	// Interval is the first polling interval. 0 means 2 seconds. It then grows up to 15 seconds; an
	// interval given above that is kept as it is.
	Interval time.Duration
	// Generation means "wait until this generation or later is observed". Used right after a
	// deploy to look only at the rollout of the generation it applied. 0 means any generation.
	Generation int64
	// OnUpdate is called with a one-line message for display whenever the state changes
	// (it is also called on the first fetch). nil does nothing.
	OnUpdate func(message string)
	// OnRetry is called when fetching the state fails and is about to be retried. nil does nothing.
	OnRetry func(err error)
}

// Wait polls until the service or job settles. It succeeds once Ready=True, and returns a failure
// as soon as Ready=False (rather than waiting for nothing).
// It returns immediately when ctx is cancelled, so Ctrl-C can interrupt it.
//
// The returned *Status is the last observed state, and it is returned on error as well (if one
// was fetched).
func (c *Client) Wait(ctx context.Context, kind Kind, name string, opts WaitOptions) (*Status, error) {
	return kind.wait(ctx, c, name, opts)
}

// subject is how wait messages name what they wait for, e.g. `service "my-svc"`.
func subject(noun, name string) string {
	return fmt.Sprintf("%s %q", noun, name)
}

// waitReady is the polling loop behind Wait, shared by every kind that settles through a Ready
// condition and observedGeneration. fetch reads the current state; subject names what is being
// waited for in the messages.
func waitReady(ctx context.Context, subject string, fetch func(context.Context) (*Status, error),
	opts WaitOptions) (*Status, error) {
	return poll(ctx, opts, poller[*Status]{
		fetch:    fetch,
		progress: func(s *Status) string { return waitProgress(s, opts.Generation) },
		done:     func(s *Status) (bool, error) { return waitDone(s, subject, opts.Generation) },
		interrupted: func(parent, waitCtx context.Context, last *Status, timeout time.Duration, lastErr error) error {
			return waitInterrupted(parent, waitCtx, subject, last, timeout, opts.Generation, lastErr)
		},
	})
}

// poller is what one wait supplies to poll: how to read the state and what to make of it.
type poller[T any] struct {
	// fetch reads the current state.
	fetch func(context.Context) (T, error)
	// progress is the one-line state shown through OnUpdate; it is reported only when it changes.
	progress func(T) string
	// done decides from a state read: whether the wait is over, and the error it ends with (nil
	// is success).
	done func(T) (bool, error)
	// endsOn, when set, ends the wait successfully on an error that is the outcome being waited
	// for (a 404 while waiting for a deletion).
	endsOn func(error) bool
	// interrupted explains a wait that ended before done said so: Ctrl-C, or the timeout. last is
	// the last state read (the zero value if none was).
	interrupted func(parent, waitCtx context.Context, last T, timeout time.Duration, lastErr error) error
}

// poll is the one polling loop behind every wait (a rollout, a deletion, an execution and its
// cancel), so they read the timeout, retry and back off the same way.
//
// A failed read is not a failed wait: the generated client does not retry, so a single transient
// 503 would otherwise make an already-applied deploy exit non-zero — the very CI false signal the
// wait exists to remove, inverted. Errors isRetryable accepts are retried until the timeout (and
// reported through OnRetry); the last one is surfaced if the wait times out. Any other error ends
// the wait at once: waiting does not fix a 403.
func poll[T any](ctx context.Context, opts WaitOptions, p poller[T]) (T, error) {
	timeout, interval := waitDefaults(opts)
	waitCtx, cancel := withWaitTimeout(ctx, timeout)
	defer cancel()

	var last T
	var previous string
	var lastErr error
	for {
		state, err := p.fetch(waitCtx)
		switch {
		case err != nil && p.endsOn != nil && p.endsOn(err):
			return last, nil
		case err == nil:
			lastErr = nil
			last = state
			if progress := p.progress(state); progress != previous {
				previous = progress
				if opts.OnUpdate != nil {
					opts.OnUpdate(progress)
				}
			}
			if done, doneErr := p.done(state); done {
				return state, doneErr
			}
		case waitCtx.Err() != nil:
			// A cancel/deadline during the wait is returned as the outcome of the wait, not as an
			// API error.
			return last, p.interrupted(ctx, waitCtx, last, timeout, lastErr)
		case !isRetryable(err):
			return last, err
		default:
			lastErr = err
			if opts.OnRetry != nil {
				opts.OnRetry(err)
			}
		}

		select {
		case <-waitCtx.Done():
			return last, p.interrupted(ctx, waitCtx, last, timeout, lastErr)
		case <-time.After(interval):
		}
		interval = nextWaitInterval(interval)
	}
}

// waitDeleted is the polling loop behind Client.WaitDeleted: it ends at the first 404.
func (k *kindOf[T]) waitDeleted(ctx context.Context, c *Client, name string, opts WaitOptions) error {
	what := subject(k.noun, name)
	_, err := poll(ctx, opts, poller[T]{
		fetch: func(ctx context.Context) (T, error) { return k.get(ctx, c, name) },
		// Gone: the outcome being waited for.
		endsOn:   isNotFound,
		progress: func(T) string { return "still present" },
		done:     func(T) (bool, error) { return false, nil },
		interrupted: func(parent, waitCtx context.Context, _ T, timeout time.Duration, lastErr error) error {
			return waitDeleteInterrupted(parent, waitCtx, what, timeout, lastErr)
		},
	})
	return err
}

// waitDeleteInterrupted builds the reason the wait for deletion was cut short.
func waitDeleteInterrupted(parent, waitCtx context.Context, subject string,
	timeout time.Duration, lastErr error) error {
	if parent.Err() != nil {
		return fmt.Errorf("interrupted while waiting for %s to be deleted: %w", subject, parent.Err())
	}
	if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
		if lastErr != nil {
			return fmt.Errorf("timed out after %s waiting for %s to be deleted; the last poll failed: %w",
				timeout, subject, lastErr)
		}
		return fmt.Errorf("timed out after %s waiting for %s to be deleted", timeout, subject)
	}
	return fmt.Errorf("stopped waiting for %s to be deleted: %w", subject, waitCtx.Err())
}

// waitDefaults fills unset parameters with their default values. Only the interval has one; a
// timeout of 0 (or less) means no limit.
func waitDefaults(opts WaitOptions) (timeout, interval time.Duration) {
	timeout, interval = max(opts.Timeout, 0), opts.Interval
	if interval <= 0 {
		interval = defaultWaitInterval
	}
	return timeout, interval
}

// withWaitTimeout is the context a wait runs under: bounded by timeout, or only by ctx when
// timeout is 0.
func withWaitTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}

// nextWaitInterval returns the next polling interval, growing it gradually up to the upper bound.
// If the user specified an interval longer than the upper bound, it is respected and not shortened
// (--interval 60s expresses "I want to hit the API less often", so cutting it down to 15s would
// increase the number of calls instead).
func nextWaitInterval(interval time.Duration) time.Duration {
	if interval >= maxWaitInterval {
		return interval
	}
	next := interval * waitBackoffNum / waitBackoffDen
	if next > maxWaitInterval {
		return maxWaitInterval
	}
	return next
}

// waitDone reports whether the wait may end in the current state. done true with a nil error is
// success; with an error it is a rollout failure. done false means keep going.
//
// As the Cloud Run documentation describes, until observedGeneration catches up to the target
// generation the conditions belong to the previous generation, so no judgement is made.
func waitDone(s *Status, subject string, generation int64) (bool, error) {
	if s.ObservedGeneration < generation {
		return false, nil
	}
	ready := s.Ready()
	if ready == nil {
		return false, nil
	}
	switch ready.Status {
	case conditionTrue:
		return true, nil
	case conditionFalse:
		return true, fmt.Errorf("%s failed to become ready: %s", subject, conditionDetail(ready))
	}
	// Unknown: not converged yet.
	return false, nil
}

// waitInterrupted builds the reason the wait was cut short. If the caller's ctx was cancelled it
// is an interruption (Ctrl-C and the like); otherwise it is a timeout.
func waitInterrupted(parent, waitCtx context.Context, subject string, last *Status,
	timeout time.Duration, generation int64, lastErr error) error {
	if parent.Err() != nil {
		return fmt.Errorf("interrupted while waiting for %s: %w", subject, parent.Err())
	}
	if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
		// If the last fetch was still failing, do not hide its cause.
		if lastErr != nil {
			return fmt.Errorf("timed out after %s waiting for %s; the last poll failed: %w",
				timeout, subject, lastErr)
		}
		return fmt.Errorf("timed out after %s waiting for %s to become ready (last seen: %s)",
			timeout, subject, waitProgress(last, generation))
	}
	return fmt.Errorf("stopped waiting for %s: %w", subject, waitCtx.Err())
}

// waitProgress returns a one-line representation of the state, used for progress display and for
// comparing states. Safe with nil.
// While the awaited generation has not been observed yet, Ready is not shown. The Ready visible
// at that point belongs to the previous generation, and showing it would be misread as "already
// finished".
func waitProgress(s *Status, generation int64) string {
	if s == nil {
		return "unknown"
	}
	if s.ObservedGeneration < generation {
		return fmt.Sprintf("generation %d not observed yet (currently %d)", generation, s.ObservedGeneration)
	}
	ready := s.Ready()
	if ready == nil {
		return fmt.Sprintf("observed generation %d, no Ready condition yet", s.ObservedGeneration)
	}
	return fmt.Sprintf("observed generation %d, Ready=%s", s.ObservedGeneration, conditionDetail(ready))
}

// conditionDetail formats a condition as "Status (Reason): Message".
func conditionDetail(c *Condition) string {
	detail := c.Status
	if c.Reason != "" {
		detail = fmt.Sprintf("%s (%s)", detail, c.Reason)
	}
	if c.Message != "" {
		detail = fmt.Sprintf("%s: %s", detail, c.Message)
	}
	return detail
}
