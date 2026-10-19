package cloudrun

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	run "google.golang.org/api/run/v1"
)

const executionPath = "/apis/run.googleapis.com/v1/namespaces/test-project/executions/my-job-abcde"

// executionObject builds an execution in the shape the real API returns it: counts left out when
// they are zero, and the Completed condition alongside the others.
func executionObject(completed, reason, message string, running, succeeded, failed int64) *run.Execution {
	conds := []*run.GoogleCloudRunV1Condition{{Type: "ResourcesAvailable", Status: "True"}}
	if completed != "" {
		conds = append(conds, &run.GoogleCloudRunV1Condition{
			Type: "Completed", Status: completed, Reason: reason, Message: message,
		})
	}
	return &run.Execution{
		Metadata: &run.ObjectMeta{Name: "my-job-abcde", Labels: map[string]string{jobLabel: "my-job"}},
		Spec:     &run.ExecutionSpec{TaskCount: 2},
		Status: &run.ExecutionStatus{
			RunningCount: running, SucceededCount: succeeded, FailedCount: failed,
			LogUri:     "https://console.cloud.google.com/logs/viewer?x",
			Conditions: conds,
		},
	}
}

func TestParseEnvVar(t *testing.T) {
	for spec, want := range map[string]EnvVar{
		"A=1":     {Name: "A", Value: "1"},
		"A=":      {Name: "A", Value: ""},
		"A=b=c":   {Name: "A", Value: "b=c"},
		" A =x y": {Name: "A", Value: "x y"},
	} {
		got, err := ParseEnvVar(spec)
		if err != nil || got != want {
			t.Errorf("ParseEnvVar(%q) = %+v, %v; want %+v", spec, got, err, want)
		}
	}
	for _, spec := range []string{"A", "=1", ""} {
		if _, err := ParseEnvVar(spec); err == nil {
			t.Errorf("ParseEnvVar(%q) = nil error, want one", spec)
		}
	}
}

func TestRunOptionsValidate(t *testing.T) {
	if err := (RunOptions{}).Validate(); err != nil {
		t.Errorf("Validate(zero) = %v", err)
	}
	tests := []struct {
		name string
		opts RunOptions
		want string
	}{
		{"negative task count", RunOptions{TaskCount: -1}, "--task-count"},
		{"negative timeout", RunOptions{TaskTimeout: -time.Second}, "--task-timeout must be positive"},
		{"fractional timeout", RunOptions{TaskTimeout: 1500 * time.Millisecond}, "whole seconds"},
		{"container with nothing to apply", RunOptions{Container: "app"}, "--container"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.opts.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// TestRunJobSendsOnlyWhatIsOverridden checks the request body: a plain run sends no overrides (it
// then needs only run.jobs.run), and overrides land where the API reads them.
func TestRunJobSendsOnlyWhatIsOverridden(t *testing.T) {
	c, api := newTestClient(t, func(*http.Request) (int, interface{}) {
		return http.StatusOK, executionObject("", "", "", 0, 0, 0)
	})

	e, err := c.RunJob(context.Background(), "my-job", RunOptions{})
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if e.Name != "my-job-abcde" || e.Job != "my-job" {
		t.Errorf("RunJob() = %+v, want the created execution", e)
	}
	post := lastRequest(t, api, http.MethodPost)
	if post.Path != myJobPath+":run" {
		t.Errorf("run path = %q, want %q", post.Path, myJobPath+":run")
	}
	if strings.Contains(string(post.Body), "overrides") {
		t.Errorf("plain run body = %s, want no overrides", post.Body)
	}

	_, err = c.RunJob(context.Background(), "my-job", RunOptions{
		Args:        []string{"migrate", "--up"},
		Env:         []EnvVar{{Name: "DRY", Value: "1"}},
		TaskCount:   3,
		TaskTimeout: 90 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunJob(overrides) error = %v", err)
	}
	var sent run.RunJobRequest
	if err := json.Unmarshal(lastRequest(t, api, http.MethodPost).Body, &sent); err != nil {
		t.Fatal(err)
	}
	o := sent.Overrides
	if o == nil || o.TaskCount != 3 || o.TimeoutSeconds != 90 || len(o.ContainerOverrides) != 1 {
		t.Fatalf("overrides = %+v", o)
	}
	co := o.ContainerOverrides[0]
	if co.Name != "" || strings.Join(co.Args, " ") != "migrate --up" || len(co.Env) != 1 ||
		co.Env[0].Name != "DRY" || co.Env[0].Value != "1" {
		t.Errorf("container override = %+v", co)
	}
}

func TestExecutionDone(t *testing.T) {
	tests := []struct {
		name     string
		e        *run.Execution
		wantDone bool
		wantErr  []string
	}{
		{name: "no Completed yet", e: executionObject("", "", "", 0, 0, 0)},
		{name: "running", e: executionObject("Unknown", "", "Waiting for execution to complete.", 1, 0, 0)},
		// A cancel passes through Unknown with a Cancelling reason; only the status decides.
		{name: "cancelling", e: executionObject("Unknown", "Cancelling", "Waiting for execution to be cancelled.", 1, 0, 0)},
		{name: "succeeded", e: executionObject("True", "", "Execution completed successfully in 11.5s.", 0, 2, 0), wantDone: true},
		{
			name:     "failed",
			e:        executionObject("False", "NonZeroExitCode", "Task my-job-abcde-task0 failed with exit code: 1", 0, 1, 1),
			wantDone: true,
			wantErr:  []string{"did not succeed", "NonZeroExitCode", "1 succeeded, 1 failed of 2", "logs: https://"},
		},
		{
			name:     "cancelled",
			e:        executionObject("False", "Cancelled", "Cancelled by user.", 0, 0, 0),
			wantDone: true,
			wantErr:  []string{"Cancelled"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done, err := executionDone(newExecution(tt.e), "my-job-abcde")
			if done != tt.wantDone {
				t.Errorf("done = %v, want %v", done, tt.wantDone)
			}
			if len(tt.wantErr) == 0 && err != nil {
				t.Errorf("error = %v, want nil", err)
			}
			for _, want := range tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to contain %q", err, want)
				}
			}
		})
	}
}

// executionAPI answers execution GETs from a sequence of states (the last one repeats) and counts
// the polls. An apiFailure in the sequence is answered as that HTTP error.
func executionAPI(t *testing.T, states ...interface{}) (*Client, func() int) {
	t.Helper()
	var mu sync.Mutex
	polls := 0
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if r.URL.Path != executionPath {
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
		mu.Lock()
		defer mu.Unlock()
		i := polls
		polls++
		if i >= len(states) {
			i = len(states) - 1
		}
		if code, ok := states[i].(apiFailure); ok {
			return int(code), googleAPIError(int(code), "boom")
		}
		return http.StatusOK, states[i]
	})
	return c, func() int {
		mu.Lock()
		defer mu.Unlock()
		return polls
	}
}

func TestWaitExecutionUntilItSucceeds(t *testing.T) {
	c, polls := executionAPI(t,
		executionObject("Unknown", "", "Waiting for execution to start.", 0, 0, 0),
		apiFailure(http.StatusServiceUnavailable), // a transient failure does not end the wait
		executionObject("True", "", "Execution completed successfully in 3s.", 0, 2, 0),
	)
	var updates []string
	retried := 0
	e, err := c.WaitExecution(context.Background(), "my-job-abcde", WaitOptions{
		Interval: time.Millisecond,
		OnUpdate: func(m string) { updates = append(updates, m) },
		OnRetry:  func(error) { retried++ },
	})
	if err != nil {
		t.Fatalf("WaitExecution() error = %v", err)
	}
	if e.Succeeded != 2 || polls() != 3 || retried != 1 {
		t.Errorf("execution = %+v after %d polls and %d retries", e, polls(), retried)
	}
	if len(updates) != 2 || !strings.Contains(updates[1], "Completed=True") {
		t.Errorf("updates = %q, want one per state", updates)
	}
}

func TestWaitExecutionFailsWithTheExecution(t *testing.T) {
	c, _ := executionAPI(t, executionObject("False", "NonZeroExitCode", "exit code 1", 0, 0, 2))
	_, err := c.WaitExecution(context.Background(), "my-job-abcde", WaitOptions{Interval: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "did not succeed") {
		t.Errorf("WaitExecution() error = %v, want the failure", err)
	}
}

// TestWaitExecutionHasNoDefaultTimeout checks that a zero Timeout does not fall back to Wait's ten
// minutes: an execution that outlives it would otherwise fail a CI step and keep running.
func TestWaitExecutionHasNoDefaultTimeout(t *testing.T) {
	c, _ := executionAPI(t, executionObject("Unknown", "", "", 1, 0, 0))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	_, err := c.WaitExecution(ctx, "my-job-abcde", WaitOptions{Interval: time.Millisecond})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitExecution() error = %v, want it to run until cancelled", err)
	}
	for _, want := range []string{"stopped waiting", "keeps running",
		"clrnd jobs executions cancel my-job-abcde --project test-project --region asia-northeast1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
}

func TestWaitExecutionTimeoutSaysItKeepsRunning(t *testing.T) {
	c, _ := executionAPI(t, executionObject("Unknown", "", "", 1, 0, 0))
	_, err := c.WaitExecution(context.Background(), "my-job-abcde",
		WaitOptions{Timeout: 20 * time.Millisecond, Interval: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out after 20ms") ||
		!strings.Contains(err.Error(), "keeps running") {
		t.Errorf("WaitExecution() error = %v, want a timeout that says the execution goes on", err)
	}
}

func TestWaitExecutionStopsOnAPermanentError(t *testing.T) {
	c, polls := executionAPI(t, apiFailure(http.StatusForbidden))
	_, err := c.WaitExecution(context.Background(), "my-job-abcde", WaitOptions{Interval: time.Millisecond})
	if err == nil || polls() != 1 {
		t.Errorf("WaitExecution() error = %v after %d polls, want it to stop at once", err, polls())
	}
}

// apiFailure is an HTTP error code for executionAPI to answer with.
type apiFailure int
