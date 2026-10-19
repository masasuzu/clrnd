package cloudrun

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	run "google.golang.org/api/run/v1"
)

// conditionCompleted is the condition that says whether an execution has finished: True when it
// succeeded, False when it failed or was cancelled, Unknown while it runs.
const conditionCompleted = "Completed"

// RunOptions overrides parts of the job for one execution only; the job itself is not changed.
// The zero value runs the job as it is defined.
type RunOptions struct {
	// Container names the container Args and Env apply to. It may be empty when the job has a
	// single container, which is then left unnamed in the request (the API applies it to that one;
	// confirmed against the real API for a job whose only container has no name).
	Container string
	// Args, when not empty, replace the container's arguments.
	Args []string
	// Env is merged into the container's environment: a variable that exists is overridden.
	Env []EnvVar
	// TaskCount, when not 0, replaces the number of tasks.
	TaskCount int64
	// TaskTimeout, when not 0, replaces how long each task may run. Whole seconds only.
	TaskTimeout time.Duration
}

// EnvVar is one environment variable override.
type EnvVar struct {
	Name  string
	Value string
}

// ParseEnvVar parses a NAME=VALUE override. The value may be empty and may itself contain "=".
func ParseEnvVar(spec string) (EnvVar, error) {
	name, value, ok := strings.Cut(spec, "=")
	if !ok || strings.TrimSpace(name) == "" {
		return EnvVar{}, fmt.Errorf("invalid --env %q: want NAME=VALUE", spec)
	}
	return EnvVar{Name: strings.TrimSpace(name), Value: value}, nil
}

// Validate checks the overrides without the API, so a bad flag fails before any credentials are
// looked for.
func (o RunOptions) Validate() error {
	var errs []error
	if o.TaskCount < 0 {
		errs = append(errs, fmt.Errorf("--task-count must be at least 1, got %d", o.TaskCount))
	}
	switch {
	case o.TaskTimeout < 0:
		errs = append(errs, fmt.Errorf("--task-timeout must be positive, got %s", o.TaskTimeout))
	case o.TaskTimeout%time.Second != 0:
		// The API takes whole seconds. Rounding silently would run the task with a timeout nobody
		// asked for.
		errs = append(errs, fmt.Errorf("--task-timeout must be whole seconds, got %s", o.TaskTimeout))
	}
	if o.Container != "" && len(o.Args) == 0 && len(o.Env) == 0 {
		errs = append(errs, errors.New("--container names the container --args and --env apply to, "+
			"but neither was given"))
	}
	return errors.Join(errs...)
}

// overrides converts the options into the request's overrides. nil when nothing is overridden, so
// a plain run sends no overrides at all (and needs only run.jobs.run, not runWithOverrides).
func (o RunOptions) overrides() *run.Overrides {
	out := &run.Overrides{TaskCount: o.TaskCount, TimeoutSeconds: int64(o.TaskTimeout / time.Second)}
	if len(o.Args) > 0 || len(o.Env) > 0 {
		c := &run.ContainerOverride{Name: o.Container, Args: o.Args}
		for _, e := range o.Env {
			c.Env = append(c.Env, &run.EnvVar{Name: e.Name, Value: e.Value})
		}
		out.ContainerOverrides = []*run.ContainerOverride{c}
	}
	if out.TaskCount == 0 && out.TimeoutSeconds == 0 && out.ContainerOverrides == nil {
		return nil
	}
	return out
}

// Execution is the state of one run of a job. It holds only read-only information taken from the
// API response.
type Execution struct {
	Name string `json:"name"`
	// Status is what the execution is doing or how it ended: Pending, Running, Cancelling,
	// Succeeded, Failed or Cancelled (see executionStatus).
	Status         string      `json:"status"`
	Job            string      `json:"job,omitempty"`
	Created        string      `json:"created,omitempty"`
	TaskCount      int64       `json:"taskCount,omitempty"`
	Running        int64       `json:"running"`
	Succeeded      int64       `json:"succeeded"`
	Failed         int64       `json:"failed"`
	Cancelled      int64       `json:"cancelled"`
	Retried        int64       `json:"retried"`
	StartTime      string      `json:"startTime,omitempty"`
	CompletionTime string      `json:"completionTime,omitempty"`
	LogURI         string      `json:"logUri,omitempty"`
	Conditions     []Condition `json:"conditions,omitempty"`
}

// jobLabel is the label an execution carries naming its job.
const jobLabel = "run.googleapis.com/job"

// cancelTimeLabel is the label Cloud Run puts on an execution once a cancel has been requested.
// Completed stays Unknown, with no reason, for a while after that, so the label is what tells a
// cancelling execution from a running one.
const cancelTimeLabel = "run.googleapis.com/cancelTime"

// newExecution converts an API response into an Execution. It is pure, so the judging and the
// messages built on it are testable without the API. The counts are omitted by the API when they
// are zero, which the zero values here reproduce.
func newExecution(obj *run.Execution) *Execution {
	e := &Execution{}
	if obj == nil {
		e.Status = executionStatus(e, false)
		return e
	}
	cancelRequested := false
	if obj.Metadata != nil {
		e.Name = obj.Metadata.Name
		e.Job = obj.Metadata.Labels[jobLabel]
		e.Created = obj.Metadata.CreationTimestamp
		_, cancelRequested = obj.Metadata.Labels[cancelTimeLabel]
	}
	if obj.Spec != nil {
		e.TaskCount = obj.Spec.TaskCount
	}
	if s := obj.Status; s != nil {
		e.Running = s.RunningCount
		e.Succeeded = s.SucceededCount
		e.Failed = s.FailedCount
		e.Cancelled = s.CancelledCount
		e.Retried = s.RetriedCount
		e.StartTime = s.StartTime
		e.CompletionTime = s.CompletionTime
		e.LogURI = s.LogUri
		e.Conditions = newConditions(s.Conditions)
	}
	e.Status = executionStatus(e, cancelRequested)
	return e
}

// executionStatus names what an execution is doing, from its Completed condition. Confirmed
// against the real API: True is a success; False with reason Cancelled is a cancel and any other
// reason (NonZeroExitCode, ...) a failure; Unknown is still going, and a cancel that was requested
// but has not taken effect yet is also Unknown, with no reason, until it settles.
//
// The cancel label only fills in for a missing reason. A task can fail while a cancel is pending,
// and the execution then ends False with that failure's reason (NonZeroExitCode, ...) and the
// label both; calling it Cancelled would hide the failure.
func executionStatus(e *Execution, cancelRequested bool) string {
	completed := e.Completed()
	switch {
	case completed == nil:
		return "Pending"
	case completed.Status == conditionTrue:
		return "Succeeded"
	case completed.Status == conditionFalse &&
		(completed.Reason == "Cancelled" || (completed.Reason == "" && cancelRequested)):
		return "Cancelled"
	case completed.Status == conditionFalse:
		return "Failed"
	case cancelRequested:
		return "Cancelling"
	}
	return "Running"
}

// Finished reports whether the execution has ended, whichever way.
func (e *Execution) Finished() bool {
	completed := e.Completed()
	return completed != nil && (completed.Status == conditionTrue || completed.Status == conditionFalse)
}

// Completed returns the Completed condition, or nil if there is none yet.
func (e *Execution) Completed() *Condition {
	for i := range e.Conditions {
		if e.Conditions[i].Type == conditionCompleted {
			return &e.Conditions[i]
		}
	}
	return nil
}

// RunJob starts an execution of the job and returns it as created (not yet finished).
func (c *Client) RunJob(ctx context.Context, job string, opts RunOptions) (*Execution, error) {
	req := &run.RunJobRequest{Overrides: opts.overrides()}
	obj, err := c.api.Namespaces.Jobs.Run(c.jobName(job), req).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to run job %q: %w", job, err)
	}
	return newExecution(obj), nil
}

// executionName builds the execution resource name for the namespaces API.
func (c *Client) executionName(execution string) string {
	return fmt.Sprintf("namespaces/%s/executions/%s", c.project, execution)
}

// GetExecution fetches the current state of an execution.
func (c *Client) GetExecution(ctx context.Context, execution string) (*Execution, error) {
	obj, err := c.api.Namespaces.Executions.Get(c.executionName(execution)).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to get execution %q: %w", execution, err)
	}
	return newExecution(obj), nil
}

// WaitExecution polls until the execution finishes. It succeeds when Completed becomes True and
// fails when it becomes False (a failed or cancelled execution).
//
// As in every wait, a zero Timeout means no limit — and here that is also the CLI's default: an
// execution can legitimately run for hours, and is bounded anyway by the job's own task timeout and
// retries. Giving up on the CLI side would not stop the execution, so a CLI-side default would only
// turn a job that later succeeds into a failed CI run. Transient failures to read the state are
// retried, as in Wait.
//
// Stopping the wait — Ctrl-C, or the timeout when one is given — never cancels the execution. The
// error says so and how to cancel it, because "the command ended" is easily read as "the job
// stopped".
func (c *Client) WaitExecution(ctx context.Context, execution string, opts WaitOptions) (*Execution, error) {
	return c.pollExecution(ctx, execution, opts, executionDone, c.executionWaitInterrupted)
}

// WaitExecutionStopped polls until an execution that was asked to cancel has stopped. It succeeds
// once Completed is False — cancelled, or failed on its own first — and fails when the execution
// succeeded before the cancel took effect, because then nothing was cancelled. A zero Timeout
// means no limit, as in WaitExecution.
func (c *Client) WaitExecutionStopped(ctx context.Context, execution string, opts WaitOptions) (*Execution, error) {
	return c.pollExecution(ctx, execution, opts, executionStopped, c.cancelWaitInterrupted)
}

// pollExecution is the loop under WaitExecution and WaitExecutionStopped. done decides when it
// ends and how; interrupted explains a wait that ended first.
func (c *Client) pollExecution(ctx context.Context, execution string, opts WaitOptions,
	done func(*Execution, string) (bool, error),
	interrupted func(parent, waitCtx context.Context, execution string, timeout time.Duration, lastErr error) error,
) (*Execution, error) {
	return poll(ctx, opts, poller[*Execution]{
		fetch:    func(ctx context.Context) (*Execution, error) { return c.GetExecution(ctx, execution) },
		progress: executionProgress,
		done:     func(e *Execution) (bool, error) { return done(e, execution) },
		interrupted: func(parent, waitCtx context.Context, _ *Execution, timeout time.Duration, lastErr error) error {
			return interrupted(parent, waitCtx, execution, timeout, lastErr)
		},
	})
}

// executionDone reports whether the wait may end. done true with a nil error is success; with an
// error it is a failed (or cancelled) execution. Only Completed's status decides: its reason is
// not reliable while the state changes (confirmed against the real API, a cancel goes through
// Unknown/Cancelling, then Unknown with no reason, before False/Cancelled).
func executionDone(e *Execution, execution string) (bool, error) {
	completed := e.Completed()
	if completed == nil {
		return false, nil
	}
	switch completed.Status {
	case conditionTrue:
		return true, nil
	case conditionFalse:
		msg := fmt.Sprintf("execution %q did not succeed: %s (%s)", execution, conditionDetail(completed), taskCounts(e))
		if e.LogURI != "" {
			msg += "; logs: " + e.LogURI
		}
		return true, errors.New(msg)
	}
	return false, nil
}

// executionStopped reports whether a cancelled execution has stopped (see WaitExecutionStopped).
func executionStopped(e *Execution, execution string) (bool, error) {
	completed := e.Completed()
	if completed == nil {
		return false, nil
	}
	switch completed.Status {
	case conditionFalse:
		return true, nil
	case conditionTrue:
		return true, fmt.Errorf("execution %q succeeded before the cancel took effect (%s)", execution, taskCounts(e))
	}
	return false, nil
}

// executionProgress returns a one-line representation of the state for progress display and for
// noticing changes. Safe with nil.
func executionProgress(e *Execution) string {
	if e == nil {
		return "unknown"
	}
	completed := e.Completed()
	if completed == nil {
		return fmt.Sprintf("no Completed condition yet (%s)", taskCounts(e))
	}
	return fmt.Sprintf("Completed=%s (%s)", conditionDetail(completed), taskCounts(e))
}

// taskCounts summarizes the tasks of an execution, e.g. "tasks: 1 running, 0 succeeded, 0 failed
// of 3".
func taskCounts(e *Execution) string {
	s := fmt.Sprintf("tasks: %d running, %d succeeded, %d failed", e.Running, e.Succeeded, e.Failed)
	if e.Cancelled > 0 {
		s += fmt.Sprintf(", %d cancelled", e.Cancelled)
	}
	if e.TaskCount > 0 {
		s += fmt.Sprintf(" of %d", e.TaskCount)
	}
	return s
}

// executionWaitInterrupted builds the reason the wait ended before the execution did. The
// execution keeps running in every case, so each message says so and how to stop it.
func (c *Client) executionWaitInterrupted(parent, waitCtx context.Context, execution string,
	timeout time.Duration, lastErr error) error {
	still := fmt.Sprintf("the execution keeps running; to stop it: "+
		"clrnd jobs executions cancel %s --project %s --region %s", execution, c.project, c.region)
	if parent.Err() != nil {
		return fmt.Errorf("stopped waiting for execution %q (%w); %s", execution, parent.Err(), still)
	}
	if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
		if lastErr != nil {
			return fmt.Errorf("timed out after %s waiting for execution %q, and the last poll failed (%w); %s",
				timeout, execution, lastErr, still)
		}
		return fmt.Errorf("timed out after %s waiting for execution %q; %s", timeout, execution, still)
	}
	return fmt.Errorf("stopped waiting for execution %q (%w); %s", execution, waitCtx.Err(), still)
}

// cancelWaitInterrupted builds the reason the wait for a cancel ended before the execution
// stopped. The cancel has been requested either way and Cloud Run carries it out, so the message
// says that rather than suggesting to cancel again.
func (c *Client) cancelWaitInterrupted(parent, waitCtx context.Context, execution string,
	timeout time.Duration, lastErr error) error {
	const pending = "the cancel was requested and Cloud Run carries it out; " +
		"clrnd jobs executions list shows when it has stopped"
	if parent.Err() != nil {
		return fmt.Errorf("stopped waiting for execution %q to stop (%w); %s", execution, parent.Err(), pending)
	}
	if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
		if lastErr != nil {
			return fmt.Errorf("timed out after %s waiting for execution %q to stop, and the last poll failed (%w); %s",
				timeout, execution, lastErr, pending)
		}
		return fmt.Errorf("timed out after %s waiting for execution %q to stop; %s", timeout, execution, pending)
	}
	return fmt.Errorf("stopped waiting for execution %q to stop (%w); %s", execution, waitCtx.Err(), pending)
}
