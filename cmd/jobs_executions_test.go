package cmd

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

const (
	executionsPath = "/apis/run.googleapis.com/v1/namespaces/test-project/executions"
	myExecPath     = executionsPath + "/my-job-abcde"
)

// listedExecutionJSON is an execution as the API returns it, with its Completed condition and, when
// cancelRequested, the label Cloud Run adds once a cancel has been asked for.
func listedExecutionJSON(name, created, completed, reason string, cancelRequested bool) string {
	labels := `"run.googleapis.com/job": "my-job"`
	if cancelRequested {
		labels += `, "run.googleapis.com/cancelTime": "2026-01-02T03:04:05Z"`
	}
	return `{"metadata": {"name": "` + name + `", "creationTimestamp": "` + created + `", "labels": {` + labels + `}},
  "spec": {"taskCount": 1},
  "status": {"runningCount": 1, "conditions": [{"type": "Completed", "status": "` + completed + `", "reason": "` + reason + `"}]}}`
}

func TestJobsExecutionsListEndToEnd(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case jobPath:
			_, _ = w.Write([]byte(liveJobWithExecutionsJSON))
		case executionsPath:
			if !strings.Contains(r.URL.RawQuery, "labelSelector=run.googleapis.com%2Fjob%3Dmy-job") {
				t.Errorf("list query = %q, want it filtered by the job", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"items": [` +
				listedExecutionJSON("my-job-old", "2026-01-01T00:00:00Z", "True", "", false) + `,` +
				listedExecutionJSON("my-job-new", "2026-01-02T00:00:00Z", "Unknown", "", false) + `]}`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
	})

	stdout, _, err := executeRoot(t, "jobs", "executions", "list", "my-job",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("jobs executions list error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "my-job-new") || !strings.Contains(lines[1], "Running") ||
		!strings.HasPrefix(lines[2], "my-job-old") || !strings.Contains(lines[2], "Succeeded") {
		t.Errorf("jobs executions list =\n%s\nwant the header, then newest first", stdout)
	}

	stdout, _, err = executeRoot(t, "jobs", "executions", "list", "my-job", "--format", "json",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("jobs executions list --format json error = %v", err)
	}
	var got []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("jobs executions list --format json printed %q: %v", stdout, err)
	}
	if len(got) != 2 || got[0].Name != "my-job-new" || got[0].Status != "Running" {
		t.Errorf("jobs executions list JSON = %+v", got)
	}
}

// startCancelAPI serves a running execution that becomes cancelled after the cancel, and records
// the requests.
func startCancelAPI(t *testing.T, completed string) func() []string {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	cancelled := false
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == myExecPath+":cancel":
			cancelled = true
			_, _ = w.Write([]byte(listedExecutionJSON("my-job-abcde", "2026-01-02T00:00:00Z", "Unknown", "", true)))
		case r.Method == http.MethodGet && r.URL.Path == myExecPath && cancelled:
			_, _ = w.Write([]byte(listedExecutionJSON("my-job-abcde", "2026-01-02T00:00:00Z", "False", "Cancelled", true)))
		case r.Method == http.MethodGet && r.URL.Path == myExecPath:
			_, _ = w.Write([]byte(listedExecutionJSON("my-job-abcde", "2026-01-02T00:00:00Z", completed, "", false)))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error": {"code": 404, "message": "not found"}}`))
		}
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func TestJobsExecutionsCancelEndToEnd(t *testing.T) {
	calls := startCancelAPI(t, "Unknown")
	stdout, stderr, err := executeRoot(t, "jobs", "executions", "cancel", "my-job-abcde", "--auto-approve",
		"--interval", "1ms", "--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("jobs executions cancel error = %v", err)
	}
	for _, want := range []string{"About to cancel:", "execution: my-job-abcde", "job:       my-job",
		"project:   test-project", "status:    Running (0/1 succeeded, 1 running)",
		"waiting for my-job-abcde to stop: Completed=False (Cancelled)"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not contain %q:\n%s", want, stderr)
		}
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if got := strings.Join(calls(), ","); !strings.Contains(got, "POST "+myExecPath+":cancel") {
		t.Errorf("requests = %s, want the cancel", got)
	}
}

func TestJobsExecutionsCancelRefusesWithoutConfirmation(t *testing.T) {
	calls := startCancelAPI(t, "Unknown")
	_, _, err := executeRoot(t, "jobs", "executions", "cancel", "my-job-abcde",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil || !strings.Contains(err.Error(), "refusing to cancel without confirmation") {
		t.Errorf("error = %v, want a refusal", err)
	}
	for _, c := range calls() {
		if strings.HasPrefix(c, "POST") {
			t.Errorf("request %q cancelled without confirmation", c)
		}
	}
}

// TestJobsExecutionsCancelRefusesAFinishedExecution checks that a finished execution is refused
// with its state, before the prompt and without the API's bare 400.
func TestJobsExecutionsCancelRefusesAFinishedExecution(t *testing.T) {
	calls := startCancelAPI(t, "True")
	_, stderr, err := executeRoot(t, "jobs", "executions", "cancel", "my-job-abcde", "--auto-approve",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil || !strings.Contains(err.Error(), `execution "my-job-abcde" has already finished (Succeeded)`) {
		t.Errorf("error = %v, want the execution reported as finished", err)
	}
	if strings.Contains(stderr, "About to cancel") {
		t.Errorf("stderr = %q, want nothing about to be cancelled", stderr)
	}
	for _, c := range calls() {
		if strings.HasPrefix(c, "POST") {
			t.Errorf("request %q cancelled a finished execution", c)
		}
	}
}

func TestJobsExecutionsCancelFailsWhenTheExecutionIsMissing(t *testing.T) {
	startCancelAPI(t, "Unknown")
	_, _, err := executeRoot(t, "jobs", "executions", "cancel", "my-job-nope", "--auto-approve",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil || !strings.Contains(err.Error(), `failed to get execution "my-job-nope"`) {
		t.Errorf("error = %v, want the missing execution reported", err)
	}
}

func TestJobsExecutionsRejectsAnUnknownCommand(t *testing.T) {
	_, _, err := executeRoot(t, "jobs", "executions", "describe", "my-job-abcde")
	if err == nil || !strings.Contains(err.Error(), `unknown command "describe"`) {
		t.Errorf("error = %v, want an unknown command", err)
	}
	if _, _, err := executeRoot(t, "jobs", "executions", "cancel"); err == nil {
		t.Error("cancel with no execution succeeded, want an argument error")
	}
}
