package cloudrun

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	run "google.golang.org/api/run/v1"
)

const executionsPath = "/apis/run.googleapis.com/v1/namespaces/test-project/executions"

func TestExecutionStatus(t *testing.T) {
	for _, tc := range []struct {
		name            string
		completed       string
		reason          string
		cancelRequested bool
		want            string
	}{
		{"no Completed yet", "", "", false, "Pending"},
		{"running", "Unknown", "", false, "Running"},
		// What the real API shows right after a cancel: still Unknown, no reason, plus the label.
		{"cancel requested", "Unknown", "", true, "Cancelling"},
		{"succeeded", "True", "", false, "Succeeded"},
		{"failed", "False", "NonZeroExitCode", false, "Failed"},
		{"cancelled", "False", "Cancelled", true, "Cancelled"},
		{"cancelled, reason not filled in", "False", "", true, "Cancelled"},
		// A task that failed while the cancel was pending: the failure is what happened.
		{"failed with a cancel pending", "False", "NonZeroExitCode", true, "Failed"},
		{"ended with no reason and no cancel", "False", "", false, "Failed"},
	} {
		obj := executionObject(tc.completed, tc.reason, "", 0, 0, 0)
		if tc.cancelRequested {
			obj.Metadata.Labels[cancelTimeLabel] = "2026-01-02T03:04:05Z"
		}
		if got := newExecution(obj).Status; got != tc.want {
			t.Errorf("%s: Status = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := newExecution(nil).Status; got != "Pending" {
		t.Errorf("newExecution(nil).Status = %q, want Pending", got)
	}
}

func TestExecutionFinished(t *testing.T) {
	for completed, want := range map[string]bool{"": false, "Unknown": false, "True": true, "False": true} {
		if got := newExecution(executionObject(completed, "", "", 0, 0, 0)).Finished(); got != want {
			t.Errorf("Completed=%q: Finished() = %v, want %v", completed, got, want)
		}
	}
}

// listedExecution is an execution as the list returns it.
func listedExecution(name, created, completed string) *run.Execution {
	obj := executionObject(completed, "", "", 0, 0, 0)
	obj.Metadata.Name = name
	obj.Metadata.CreationTimestamp = created
	return obj
}

func TestListExecutions(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if r.URL.Path == myJobPath {
			return http.StatusOK, liveJob("gcr.io/project/job:v1")
		}
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		// Two pages, and the second page holds the newest: the order must not be left to the API.
		if !strings.Contains(r.URL.RawQuery, "continue=") {
			return http.StatusOK, &run.ListExecutionsResponse{
				Items:    []*run.Execution{listedExecution("my-job-old", "2026-01-01T00:00:00Z", "True")},
				Metadata: &run.ListMeta{Continue: "page-2"},
			}
		}
		return http.StatusOK, &run.ListExecutionsResponse{Items: []*run.Execution{
			listedExecution("my-job-new", "2026-01-03T00:00:00.123456Z", "Unknown"),
			listedExecution("my-job-mid", "2026-01-02T00:00:00Z", "False"),
		}}
	})
	got, err := c.ListExecutions(context.Background(), "my-job")
	if err != nil {
		t.Fatalf("ListExecutions() error = %v", err)
	}
	var names []string
	for _, e := range got {
		names = append(names, e.Name+"="+e.Status)
	}
	if want := "my-job-new=Running,my-job-mid=Failed,my-job-old=Succeeded"; strings.Join(names, ",") != want {
		t.Errorf("executions = %s, want %s", strings.Join(names, ","), want)
	}
	if len(queries) != 2 || !strings.Contains(queries[0], "labelSelector=run.googleapis.com%2Fjob%3Dmy-job") {
		t.Errorf("list queries = %q, want two pages filtered by the job label", queries)
	}
}

// TestListExecutionsOfAMissingJob checks that a job that does not exist is an error, not an empty
// list that reads as "it never ran".
func TestListExecutionsOfAMissingJob(t *testing.T) {
	c, api := newTestClient(t, nil) // every request is a 404
	_, err := c.ListExecutions(context.Background(), "my-job")
	if err == nil || !strings.Contains(err.Error(), `failed to get job "my-job"`) {
		t.Errorf("ListExecutions() error = %v, want the missing job reported", err)
	}
	for _, r := range api.recorded() {
		if r.Path == executionsPath {
			t.Errorf("listed executions of a job that does not exist")
		}
	}
}

func TestListExecutionsStopsWhenPagingDoesNotAdvance(t *testing.T) {
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if r.URL.Path == myJobPath {
			return http.StatusOK, liveJob("gcr.io/project/job:v1")
		}
		return http.StatusOK, &run.ListExecutionsResponse{Metadata: &run.ListMeta{Continue: "same"}}
	})
	if _, err := c.ListExecutions(context.Background(), "my-job"); err == nil ||
		!strings.Contains(err.Error(), "pagination did not advance") {
		t.Errorf("ListExecutions() error = %v, want the paging failure", err)
	}
}

func TestExecutionsText(t *testing.T) {
	if got := (Executions{}).Text(); got != "" {
		t.Errorf("Text() of no executions = %q, want empty", got)
	}
	es := Executions{
		{Name: "my-job-b", Status: "Running", TaskCount: 3, Running: 1, Failed: 1, Created: "2026-01-02T00:00:00Z"},
		{Name: "my-job-a", Status: "Succeeded", TaskCount: 1, Succeeded: 1, Created: "2026-01-01T00:00:00Z",
			CompletionTime: "2026-01-01T00:01:00Z"},
	}
	want := "" +
		"EXECUTION  STATUS     TASKS                               CREATED               COMPLETED\n" +
		"my-job-b   Running    0/3 succeeded, 1 running, 1 failed  2026-01-02T00:00:00Z  -\n" +
		"my-job-a   Succeeded  1/1 succeeded                       2026-01-01T00:00:00Z  2026-01-01T00:01:00Z\n"
	if got := es.Text(); got != want {
		t.Errorf("Text() =\n%s\nwant\n%s", got, want)
	}
}

func TestCancelExecution(t *testing.T) {
	c, api := newTestClient(t, func(*http.Request) (int, interface{}) {
		return http.StatusOK, executionObject("Unknown", "", "", 1, 0, 0)
	})
	if err := c.CancelExecution(context.Background(), "my-job-abcde"); err != nil {
		t.Fatalf("CancelExecution() error = %v", err)
	}
	post := lastRequest(t, api, http.MethodPost)
	if post.Path != executionPath+":cancel" || strings.Contains(post.Query, "dryRun") {
		t.Errorf("cancel = %s?%s, want %s:cancel", post.Path, post.Query, executionPath)
	}
}

func TestCancelExecutionReportsTheAPIError(t *testing.T) {
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) {
		return http.StatusBadRequest, googleAPIError(400,
			"Execution 'my-job-abcde' cannot be cancelled because it is not running.")
	})
	err := c.CancelExecution(context.Background(), "my-job-abcde")
	if err == nil || !strings.Contains(err.Error(), `failed to cancel execution "my-job-abcde"`) ||
		!strings.Contains(err.Error(), "not running") {
		t.Errorf("CancelExecution() error = %v, want the API's reason", err)
	}
}

// TestWaitExecutionStopped follows the sequence the real API showed for a cancel: Unknown with no
// reason for a while, then False/Cancelled.
func TestWaitExecutionStopped(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) {
		mu.Lock()
		defer mu.Unlock()
		polls++
		if polls < 3 {
			return http.StatusOK, executionObject("Unknown", "", "", 1, 0, 0)
		}
		return http.StatusOK, executionObject("False", "Cancelled", "Cancelled by user.", 0, 0, 0)
	})
	e, err := c.WaitExecutionStopped(context.Background(), "my-job-abcde", WaitOptions{Interval: time.Millisecond})
	if err != nil {
		t.Fatalf("WaitExecutionStopped() error = %v", err)
	}
	if e.Status != "Cancelled" || polls != 3 {
		t.Errorf("WaitExecutionStopped() = %s after %d polls, want Cancelled after 3", e.Status, polls)
	}
}

func TestWaitExecutionStoppedFailsWhenItSucceededFirst(t *testing.T) {
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) {
		return http.StatusOK, executionObject("True", "", "", 0, 2, 0)
	})
	_, err := c.WaitExecutionStopped(context.Background(), "my-job-abcde", WaitOptions{Interval: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "succeeded before the cancel took effect") {
		t.Errorf("WaitExecutionStopped() error = %v, want it to say nothing was cancelled", err)
	}
}

// TestWaitExecutionStoppedTimeoutSaysTheCancelGoesOn checks that giving up on the wait does not
// suggest cancelling again: the cancel has been requested and Cloud Run carries it out.
func TestWaitExecutionStoppedTimeoutSaysTheCancelGoesOn(t *testing.T) {
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) {
		return http.StatusOK, executionObject("Unknown", "", "", 1, 0, 0)
	})
	_, err := c.WaitExecutionStopped(context.Background(), "my-job-abcde",
		WaitOptions{Timeout: 20 * time.Millisecond, Interval: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out") ||
		!strings.Contains(err.Error(), "the cancel was requested") || strings.Contains(err.Error(), "to stop it:") {
		t.Errorf("WaitExecutionStopped() error = %v", err)
	}
}
