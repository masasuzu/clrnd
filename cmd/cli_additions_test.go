package cmd

import (
	"net/http"
	"strings"
	"testing"
)

// TestRollbackTakesToAsRevision checks that --to is the same flag as --revision. A revision of
// another service is refused only when the flag reached rollback: by default it picks one itself.
func TestRollbackTakesToAsRevision(t *testing.T) {
	startRollbackAPI(t)
	_, _, err := executeRoot(t, "rollback", "my-svc", "--to", "other-00001-xyz", "--auto-approve", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil || !strings.Contains(err.Error(), `revision "other-00001-xyz" does not belong to this service`) {
		t.Errorf("rollback --to error = %v, want the revision refused", err)
	}
}

func TestFormatCompletesItsValues(t *testing.T) {
	stdout, _, err := executeRoot(t, "__complete", "status", "--format", "")
	if err != nil {
		t.Fatalf("__complete error = %v", err)
	}
	if !strings.HasPrefix(stdout, "text\njson\n") {
		t.Errorf("completion = %q, want text and json", stdout)
	}
}

func TestJobsWaitEndToEnd(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != jobPath {
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveJobWithExecutionsJSON))
	})
	stdout, stderr, err := executeRoot(t, "jobs", "wait", "my-job", "--interval", "1ms",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("jobs wait error = %v", err)
	}
	if stdout != "" || !strings.Contains(stderr, "waiting for my-job:") || !strings.Contains(stderr, "Ready=True") {
		t.Errorf("jobs wait stdout = %q, stderr = %q; want progress on stderr only", stdout, stderr)
	}
}

func TestJobsExecutionsWait(t *testing.T) {
	for _, tc := range []struct {
		completed, reason string
		wantErr           string
	}{
		{"True", "", ""},
		{"False", "NonZeroExitCode", "did not succeed"},
	} {
		runAPI(t, executionJSON(tc.completed, tc.reason))
		stdout, stderr, err := executeRoot(t, "jobs", "executions", "wait", "my-job-abcde", "--interval", "1ms",
			"--project", "test-project", "--region", "asia-northeast1")
		if tc.wantErr == "" && err != nil {
			t.Errorf("Completed=%s: error = %v", tc.completed, err)
		}
		if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("Completed=%s: error = %v, want %q", tc.completed, err, tc.wantErr)
		}
		if stdout != "" || !strings.Contains(stderr, "waiting for my-job-abcde: Completed="+tc.completed) {
			t.Errorf("Completed=%s: stdout = %q, stderr = %q", tc.completed, stdout, stderr)
		}
	}
	if _, _, err := executeRoot(t, "jobs", "executions", "wait"); err == nil {
		t.Error("jobs executions wait with no execution succeeded, want an argument error")
	}
}
