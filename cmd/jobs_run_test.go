package cmd

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// executionJSON builds an execution response; completed "" leaves the Completed condition out.
func executionJSON(completed, reason string) string {
	conds := `{"type": "ResourcesAvailable", "status": "True"}`
	if completed != "" {
		conds += fmt.Sprintf(`, {"type": "Completed", "status": %q, "reason": %q}`, completed, reason)
	}
	return fmt.Sprintf(`{
  "metadata": {"name": "my-job-abcde", "labels": {"run.googleapis.com/job": "my-job"}},
  "spec": {"taskCount": 1},
  "status": {"succeededCount": 1, "conditions": [%s]}
}`, conds)
}

// runAPI answers a job run with a new execution and execution GETs with final; it records the
// requests (method, path and body).
func runAPI(t *testing.T, final string) func() []string {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+" "+string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == jobPath+":run":
			_, _ = w.Write([]byte(executionJSON("", "")))
		case r.Method == http.MethodGet &&
			r.URL.Path == "/apis/run.googleapis.com/v1/namespaces/test-project/executions/my-job-abcde":
			_, _ = w.Write([]byte(final))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func TestJobsRunEndToEnd(t *testing.T) {
	calls := runAPI(t, executionJSON("True", ""))

	stdout, stderr, err := executeRoot(t, "jobs", "run", "my-job",
		"--project", "test-project", "--region", "asia-northeast1", "--interval", "1ms")
	if err != nil {
		t.Fatalf("jobs run error = %v", err)
	}
	if stdout != "my-job-abcde\n" {
		t.Errorf("stdout = %q, want only the execution name", stdout)
	}
	if !strings.Contains(stderr, "waiting for my-job-abcde: Completed=True") {
		t.Errorf("stderr = %q, want the wait reported", stderr)
	}
	if !strings.Contains(stderr, "job:     my-job") || !strings.Contains(stderr, "project: test-project") {
		t.Errorf("stderr = %q, want the job and project it runs in", stderr)
	}
	if got := calls(); len(got) < 2 || strings.Contains(got[0], "overrides") {
		t.Errorf("requests = %q, want a plain run, then the wait", got)
	}
}

// TestJobsRunFailsWithTheExecution is the point of waiting: a failed execution fails the command,
// so a CI step that runs a migration fails with it.
func TestJobsRunFailsWithTheExecution(t *testing.T) {
	runAPI(t, executionJSON("False", "NonZeroExitCode"))

	stdout, _, err := executeRoot(t, "jobs", "run", "my-job",
		"--project", "test-project", "--region", "asia-northeast1", "--interval", "1ms")
	if err == nil || !strings.Contains(err.Error(), `execution "my-job-abcde" did not succeed`) {
		t.Errorf("jobs run error = %v, want the failed execution", err)
	}
	if stdout != "my-job-abcde\n" {
		t.Errorf("stdout = %q, want the execution name even on failure", stdout)
	}
}

func TestJobsExecuteIsAnAlias(t *testing.T) {
	calls := runAPI(t, executionJSON("True", ""))
	if _, _, err := executeRoot(t, "jobs", "execute", "my-job", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("jobs execute error = %v", err)
	}
	// --no-wait: the run and nothing after it.
	if got := calls(); len(got) != 1 || !strings.HasPrefix(got[0], "POST "+jobPath+":run") {
		t.Errorf("requests = %q, want only the run", got)
	}
}

func TestJobsRunSendsTheOverrides(t *testing.T) {
	calls := runAPI(t, executionJSON("True", ""))
	_, _, err := executeRoot(t, "jobs", "run", "my-job", "--no-wait",
		"--args", "migrate", "--args", "--up", "--env", "DRY=1", "--task-count", "2", "--task-timeout", "90s",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("jobs run error = %v", err)
	}
	body := calls()[0]
	for _, want := range []string{`"args":["migrate","--up"]`, `"name":"DRY","value":"1"`,
		`"taskCount":2`, `"timeoutSeconds":90`} {
		if !strings.Contains(body, want) {
			t.Errorf("run request %s does not contain %s", body, want)
		}
	}
}

// TestJobsRunChecksFlagsFirst checks that a bad override fails before the target is resolved or
// the API is called, like every other local check.
func TestJobsRunChecksFlagsFirst(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected API call to %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	for _, tt := range []struct {
		flags []string
		want  string
	}{
		{[]string{"--env", "NOVALUE"}, "want NAME=VALUE"},
		{[]string{"--task-count", "0"}, "--task-count must be at least 1"},
		{[]string{"--task-timeout", "0s"}, "--task-timeout must be positive"},
		{[]string{"--task-timeout", "1500ms"}, "whole seconds"},
		{[]string{"--container", "app"}, "neither was given"},
		{[]string{"--timeout", "-1s"}, "--timeout must not be negative"},
	} {
		t.Run(strings.Join(tt.flags, " "), func(t *testing.T) {
			// No --project / --region: a check that ran after target resolution would fail on
			// "project is required" instead.
			_, _, err := executeRoot(t, append([]string{"jobs", "run", "my-job"}, tt.flags...)...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}
