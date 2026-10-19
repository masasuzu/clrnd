package cmd

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// liveJobWithExecutionsJSON is liveJobJSON after a few runs.
const liveJobWithExecutionsJSON = `{
  "apiVersion": "run.googleapis.com/v1",
  "kind": "Job",
  "metadata": {"name": "my-job", "namespace": "test-project", "generation": 4, "resourceVersion": "rv-4",
    "labels": {"run.googleapis.com/lastUpdatedTime": "2026-01-02T03:04:05Z"}},
  "spec": {"template": {"spec": {"template": {"spec": {"containers": [{"image": "gcr.io/project/job:old"}]}}}}},
  "status": {
    "observedGeneration": 4,
    "conditions": [{"type": "Ready", "status": "True"}],
    "executionCount": 3,
    "latestCreatedExecution": {"name": "my-job-abcde", "completionStatus": "EXECUTION_SUCCEEDED"}
  }
}`

func TestJobsStatusEndToEnd(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != jobPath {
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveJobWithExecutionsJSON))
	})

	stdout, _, err := executeRoot(t, "jobs", "status", "my-job",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("jobs status error = %v", err)
	}
	for _, want := range []string{"Job:             my-job", "Ready:           True", "Executions:      3",
		"Last execution:  my-job-abcde (Succeeded)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("jobs status stdout does not contain %q:\n%s", want, stdout)
		}
	}

	stdout, _, err = executeRoot(t, "jobs", "status", "my-job", "--format", "json",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("jobs status --format json error = %v", err)
	}
	var got struct {
		Job             string `json:"job"`
		ExecutionCount  int    `json:"executionCount"`
		LatestExecution struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"latestExecution"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("jobs status --format json printed %q: %v", stdout, err)
	}
	if got.Job != "my-job" || got.ExecutionCount != 3 || got.LatestExecution.Status != "Succeeded" {
		t.Errorf("jobs status JSON = %+v", got)
	}
}

// startJobDeleteAPI serves the job until it is deleted, then 404s, and counts the DELETEs.
func startJobDeleteAPI(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	deleted := false
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete:
			deleted = !strings.Contains(r.URL.RawQuery, "dryRun")
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && !deleted:
			_, _ = w.Write([]byte(liveJobWithExecutionsJSON))
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

func TestJobsDeleteEndToEnd(t *testing.T) {
	calls := startJobDeleteAPI(t)

	stdout, stderr, err := executeRoot(t, "jobs", "delete", "my-job", "--auto-approve", "--interval", "1ms",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("jobs delete error = %v", err)
	}
	for _, want := range []string{"About to delete:", "job:        my-job", "project:    test-project",
		"region:     asia-northeast1", "executions: 3 (deleted with the job)"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("jobs delete stderr does not contain %q:\n%s", want, stderr)
		}
	}
	if stdout != "" {
		t.Errorf("jobs delete stdout = %q, want empty", stdout)
	}
	got := calls()
	if len(got) < 3 || got[1] != "DELETE "+jobPath+"?alt=json&prettyPrint=false" ||
		!strings.HasPrefix(got[len(got)-1], "GET "+jobPath) {
		t.Errorf("requests = %q, want a lookup, the delete, then polls until it is gone", got)
	}
}

func TestJobsDeleteRefusesWithoutConfirmation(t *testing.T) {
	calls := startJobDeleteAPI(t)
	_, _, err := executeRoot(t, "jobs", "delete", "my-job",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil || !strings.Contains(err.Error(), "refusing to delete without confirmation") {
		t.Errorf("jobs delete error = %v, want a refusal", err)
	}
	for _, c := range calls() {
		if strings.HasPrefix(c, "DELETE") {
			t.Errorf("request %q deleted the job without confirmation", c)
		}
	}
}

func TestJobsDeleteFailsWhenTheJobIsMissing(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			t.Error("a missing job was deleted")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error": {"code": 404, "message": "not found"}}`))
	})
	_, _, err := executeRoot(t, "jobs", "delete", "my-job", "--auto-approve",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil || !strings.Contains(err.Error(), `failed to get job "my-job"`) {
		t.Errorf("jobs delete error = %v, want the missing job reported", err)
	}
}

// TestJobsInitThenDiffIsEmpty checks that jobs init writes a config naming the job and a manifest
// the jobs commands accept as is: the diff against the job it came from is empty.
func TestJobsInitThenDiffIsEmpty(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if echoDryRun(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveJobWithExecutionsJSON))
	})
	dir := t.TempDir()
	t.Chdir(dir)

	if _, _, err := executeRoot(t, "jobs", "init", "my-job",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("jobs init error = %v", err)
	}
	config, err := os.ReadFile(filepath.Join(dir, "clrnd.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "job: my-job") || strings.Contains(string(config), "service:") {
		t.Errorf("clrnd.yml =\n%s\nwant it to name the job", config)
	}

	stdout, _, err := executeRoot(t, "jobs", "diff")
	if err != nil {
		t.Fatalf("jobs diff error = %v", err)
	}
	if stdout != "" {
		t.Errorf("jobs diff right after jobs init = %q, want empty", stdout)
	}
	// And the service commands leave it alone.
	if _, _, err := executeRoot(t, "diff"); err == nil ||
		!strings.Contains(err.Error(), "the config file names a job") {
		t.Errorf("diff error = %v, want it to refuse the job's config", err)
	}
}

// TestJobsRejectsAnUnknownCommand checks that a mistyped jobs command fails instead of printing the
// help and exiting 0. load is the case in point: it is init's old name for services only.
func TestJobsRejectsAnUnknownCommand(t *testing.T) {
	_, _, err := executeRoot(t, "jobs", "load", "my-job")
	if err == nil || !strings.Contains(err.Error(), `unknown command "load"`) {
		t.Errorf("jobs load error = %v, want an unknown command", err)
	}
	if _, _, err := executeRoot(t, "jobs"); err != nil {
		t.Errorf("jobs error = %v, want the help", err)
	}
}
