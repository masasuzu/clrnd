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

// Default values for the wait parameters.
const (
	defaultWaitTimeout  = 10 * time.Minute
	defaultWaitInterval = 2 * time.Second
	// Upper bound on the polling interval. The interval grows gradually and then levels off, so
	// the API is not hit continuously while waiting out a long startup.
	maxWaitInterval = 15 * time.Second
	waitBackoffNum  = 3
	waitBackoffDen  = 2
)

// WaitOptions controls how Wait behaves. The zero value is usable (defaults are filled in).
type WaitOptions struct {
	// Timeout is the upper bound on the whole wait. 0 means 10 minutes.
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

// Wait polls until the service settles. It succeeds once Ready=True, and returns a failure as
// soon as Ready=False (rather than waiting for nothing).
// It returns immediately when ctx is cancelled, so Ctrl-C can interrupt it.
//
// The returned *Status is the last observed state, and it is returned on error as well (if one
// was fetched).
func (c *Client) Wait(ctx context.Context, service string, opts WaitOptions) (*Status, error) {
	timeout, interval := waitDefaults(opts)
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var last *Status
	var previous string
	// lastErr is an error where "observing failed, but a retry might recover".
	// Giving up the wait on it would make deploy report a failure even though the apply itself
	// succeeded. That would create, inverted, exactly the CI misjudgement this wait exists to
	// prevent.
	var lastErr error
	for {
		status, err := c.Status(waitCtx, service)
		switch {
		case err == nil:
			lastErr = nil
			last = status

			if progress := waitProgress(status, opts.Generation); progress != previous {
				previous = progress
				if opts.OnUpdate != nil {
					opts.OnUpdate(progress)
				}
			}
			if done, doneErr := waitDone(status, service, opts.Generation); done {
				return status, doneErr
			}

		case waitCtx.Err() != nil:
			// A cancel/deadline during the wait is returned as the outcome of the wait, not as an
			// API error.
			return last, waitInterrupted(ctx, waitCtx, service, last, timeout, opts.Generation, lastErr)

		case !isRetryable(err):
			// A failure that waiting will not fix. A service that does not exist (404) will not
			// appear, and a bad request (400) or missing authentication/permission (401/403) does
			// not change with time either.
			return last, err

		default:
			// A transient failure (503/429/dropped connection, etc.). Retry until the timeout.
			lastErr = err
			if opts.OnRetry != nil {
				opts.OnRetry(err)
			}
		}

		select {
		case <-waitCtx.Done():
			return last, waitInterrupted(ctx, waitCtx, service, last, timeout, opts.Generation, lastErr)
		case <-time.After(interval):
		}

		interval = nextWaitInterval(interval)
	}
}

// WaitDeleted waits until the service is actually gone. Cloud Run deletion is asynchronous, and
// the service can still be fetched when the DELETE is accepted, so without this a procedure such
// as "delete, then recreate" races.
func (c *Client) WaitDeleted(ctx context.Context, service string, opts WaitOptions) error {
	timeout, interval := waitDefaults(opts)
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr error
	notified := false
	for {
		_, err := c.GetService(waitCtx, service)
		switch {
		case isNotFound(err):
			// Gone. This is the outcome being waited for.
			return nil
		case err == nil:
			lastErr = nil
			if !notified {
				notified = true
				if opts.OnUpdate != nil {
					opts.OnUpdate("still present")
				}
			}
		case waitCtx.Err() != nil:
			return waitDeleteInterrupted(ctx, waitCtx, service, timeout, lastErr)
		case !isRetryable(err):
			// The same classification as Wait. A failure that waiting will not fix is returned
			// right away.
			return err
		default:
			// Possibly a transient failure. Like Wait, retry until the timeout.
			lastErr = err
			if opts.OnRetry != nil {
				opts.OnRetry(err)
			}
		}

		select {
		case <-waitCtx.Done():
			return waitDeleteInterrupted(ctx, waitCtx, service, timeout, lastErr)
		case <-time.After(interval):
		}
		interval = nextWaitInterval(interval)
	}
}

// waitDeleteInterrupted builds the reason the wait for deletion was cut short.
func waitDeleteInterrupted(parent, waitCtx context.Context, service string,
	timeout time.Duration, lastErr error) error {
	if parent.Err() != nil {
		return fmt.Errorf("interrupted while waiting for service %q to be deleted: %w", service, parent.Err())
	}
	if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
		if lastErr != nil {
			return fmt.Errorf("timed out after %s waiting for service %q to be deleted; the last poll failed: %w",
				timeout, service, lastErr)
		}
		return fmt.Errorf("timed out after %s waiting for service %q to be deleted", timeout, service)
	}
	return fmt.Errorf("stopped waiting for service %q to be deleted: %w", service, waitCtx.Err())
}

// waitDefaults fills unset parameters with their default values.
func waitDefaults(opts WaitOptions) (timeout, interval time.Duration) {
	timeout, interval = opts.Timeout, opts.Interval
	if timeout <= 0 {
		timeout = defaultWaitTimeout
	}
	if interval <= 0 {
		interval = defaultWaitInterval
	}
	return timeout, interval
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
func waitDone(s *Status, service string, generation int64) (bool, error) {
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
		return true, fmt.Errorf("service %q failed to become ready: %s", service, conditionDetail(ready))
	}
	// Unknown: not converged yet.
	return false, nil
}

// waitInterrupted builds the reason the wait was cut short. If the caller's ctx was cancelled it
// is an interruption (Ctrl-C and the like); otherwise it is a timeout.
func waitInterrupted(parent, waitCtx context.Context, service string, last *Status,
	timeout time.Duration, generation int64, lastErr error) error {
	if parent.Err() != nil {
		return fmt.Errorf("interrupted while waiting for service %q: %w", service, parent.Err())
	}
	if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
		// If the last fetch was still failing, do not hide its cause.
		if lastErr != nil {
			return fmt.Errorf("timed out after %s waiting for service %q; the last poll failed: %w",
				timeout, service, lastErr)
		}
		return fmt.Errorf("timed out after %s waiting for service %q to become ready (last seen: %s)",
			timeout, service, waitProgress(last, generation))
	}
	return fmt.Errorf("stopped waiting for service %q: %w", service, waitCtx.Err())
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
