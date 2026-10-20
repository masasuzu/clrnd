package cmd

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

const workerPoolRevisionsJSON = `{"items": [
  {"metadata": {"name": "my-pool-00005-abc", "creationTimestamp": "2026-08-22T10:00:00Z"},
   "spec": {"containers": [{"image": "gcr.io/project/worker:old"}]},
   "status": {"conditions": [{"type": "Ready", "status": "True"}]}},
  {"metadata": {"name": "my-pool-00004-xyz", "creationTimestamp": "2026-08-21T10:00:00Z"},
   "spec": {"containers": [{"image": "gcr.io/project/worker:older"}]},
   "status": {"conditions": [{"type": "Ready", "status": "True", "reason": "Retired"}]}}
]}`

// workerPoolRevisionsAPI serves liveWorkerPoolJSON and its revisions, echoes writes back, and
// records every request with its body.
func workerPoolRevisionsAPI(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/revisions"):
			_, _ = w.Write([]byte(workerPoolRevisionsJSON))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(liveWorkerPoolJSON))
		default:
			_, _ = w.Write(body)
		}
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func TestWorkerPoolsRevisionsEndToEnd(t *testing.T) {
	calls := workerPoolRevisionsAPI(t)
	stdout, _, err := executeRoot(t, "worker-pools", "revisions", "my-pool",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("worker-pools revisions error = %v", err)
	}
	if !strings.Contains(stdout, "INSTANCES") || strings.Contains(stdout, "TAGS") ||
		!strings.Contains(stdout, "my-pool-00005-abc  True") {
		t.Errorf("worker-pools revisions stdout =\n%s", stdout)
	}
	var listed bool
	for _, c := range calls() {
		if strings.Contains(c, "/revisions?") && strings.Contains(c, "labelSelector=run.googleapis.com%2FworkerPool%3Dmy-pool") {
			listed = true
		}
	}
	if !listed {
		t.Errorf("requests = %q, want the revisions listed by the worker pool's label", calls())
	}
}

func TestWorkerPoolsRollbackEndToEnd(t *testing.T) {
	calls := workerPoolRevisionsAPI(t)
	stdout, stderr, err := executeRoot(t, "worker-pools", "rollback", "my-pool", "--auto-approve",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("worker-pools rollback error = %v", err)
	}
	if !strings.Contains(stdout, "revisionName: my-pool-00004-xyz") {
		t.Errorf("worker-pools rollback stdout = %q, want the split moving to the previous revision", stdout)
	}
	if !strings.Contains(stderr, "worker pool: my-pool") {
		t.Errorf("worker-pools rollback stderr = %q, want the target", stderr)
	}
	var put string
	for _, c := range calls() {
		if strings.HasPrefix(c, "PUT ") {
			put = c
		}
	}
	if !strings.Contains(put, `"instanceSplits":[{"percent":100,"revisionName":"my-pool-00004-xyz"}]`) ||
		!strings.Contains(put, `"resourceVersion":"rv-5"`) {
		t.Errorf("replace = %s, want every instance on the previous revision, against the read version", put)
	}
}

func TestWorkerPoolsRollbackHasNoTrafficWording(t *testing.T) {
	stdout, stderr, err := executeRoot(t, "worker-pools", "rollback", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if help := stdout + stderr; strings.Contains(help, "traffic") {
		t.Errorf("worker-pools rollback --help talks about traffic:\n%s", help)
	}
}
