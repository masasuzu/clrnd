package cmd

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

// startPruneAPI answers the service and the revision list, and records the names of the
// revisions that are DELETEd.
func startPruneAPI(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var deleted []string

	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			parts := strings.Split(r.URL.Path, "/")
			mu.Lock()
			deleted = append(deleted, parts[len(parts)-1])
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/revisions") {
			_, _ = w.Write([]byte(revisionsJSON))
			return
		}
		_, _ = w.Write([]byte(rollbackServiceJSON))
	})

	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), deleted...)
	}
}

// TestRevisionsPruneDeletesTheOldOnes checks that revisions older than --keep are deleted. Cloud
// Run does not delete old revisions automatically, so this is the only way to clean them up.
func TestRevisionsPruneDeletesTheOldOnes(t *testing.T) {
	deleted := startPruneAPI(t)

	stdout, _, err := executeRoot(t, "revisions", "my-svc", "--prune", "--keep", "1",
		"--auto-approve", "--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("revisions --prune error = %v", err)
	}
	if got := deleted(); len(got) != 1 || got[0] != "my-svc-00006-def" {
		t.Errorf("deleted = %v, want only the older revision", got)
	}
	// What is about to be deleted goes to stdout as data.
	if !strings.Contains(stdout, "my-svc-00006-def") {
		t.Errorf("stdout = %q, want the revisions to be listed before deleting", stdout)
	}
}

// TestRevisionsPruneKeepsTheServingRevision checks that the revision serving traffic survives
// even with --keep 0. Deleting it would take the service down.
func TestRevisionsPruneKeepsTheServingRevision(t *testing.T) {
	deleted := startPruneAPI(t)

	if _, _, err := executeRoot(t, "revisions", "my-svc", "--prune", "--keep", "0",
		"--auto-approve", "--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("revisions --prune error = %v", err)
	}
	for _, name := range deleted() {
		if name == "my-svc-00007-abc" {
			t.Errorf("deleted = %v, want the revision serving traffic to be kept", deleted())
		}
	}
}

// TestRevisionsPruneDryRunDeletesNothing checks that --dry-run only prints the list and deletes
// nothing. It does not show the confirmation prompt either (the same policy as delete).
func TestRevisionsPruneDryRunDeletesNothing(t *testing.T) {
	deleted := startPruneAPI(t)

	stdout, stderr, err := executeRoot(t, "revisions", "my-svc", "--prune", "--keep", "0", "--dry-run",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("revisions --prune --dry-run error = %v", err)
	}
	if got := deleted(); len(got) != 0 {
		t.Errorf("deleted = %v, want nothing deleted on a dry run", got)
	}
	if !strings.Contains(stdout, "my-svc-00006-def") {
		t.Errorf("stdout = %q, want the candidates listed", stdout)
	}
	if !strings.Contains(stderr, "Dry run") {
		t.Errorf("stderr = %q, want the dry run reported", stderr)
	}
}

// TestRevisionsPruneRefusesWithoutConfirmation checks that nothing is deleted in a
// non-interactive environment without --auto-approve. A destructive operation is treated the
// same way as delete.
func TestRevisionsPruneRefusesWithoutConfirmation(t *testing.T) {
	deleted := startPruneAPI(t)

	_, _, err := executeRoot(t, "revisions", "my-svc", "--prune", "--keep", "0",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil {
		t.Fatal("revisions --prune error = nil, want a refusal without a terminal")
	}
	if !strings.Contains(err.Error(), "refusing to prune without confirmation") {
		t.Errorf("error = %v", err)
	}
	if got := deleted(); len(got) != 0 {
		t.Errorf("deleted = %v, want nothing deleted", got)
	}
}

// TestRevisionsWithoutPruneStaysReadOnly checks that the command only lists revisions unless
// --prune is passed.
func TestRevisionsWithoutPruneStaysReadOnly(t *testing.T) {
	deleted := startPruneAPI(t)

	if _, _, err := executeRoot(t, "revisions", "my-svc",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("revisions error = %v", err)
	}
	if got := deleted(); len(got) != 0 {
		t.Errorf("deleted = %v, want listing to delete nothing", got)
	}
}

// TestRevisionsPruneFlagsNeedPrune checks that the pruning flags are not silently ignored when
// they are given without --prune. Ignoring them lets a run that "meant to clean up but only
// looked at the list" finish successfully.
func TestRevisionsPruneFlagsNeedPrune(t *testing.T) {
	deleted := startPruneAPI(t)

	for _, flag := range [][]string{{"--keep", "5"}, {"--auto-approve"}, {"--dry-run"}} {
		args := append([]string{"revisions", "my-svc"}, flag...)
		args = append(args, "--project", "test-project", "--region", "asia-northeast1")
		_, _, err := executeRoot(t, args...)
		if err == nil || !strings.Contains(err.Error(), "only applies with --prune") {
			t.Errorf("revisions %v error = %v, want it to reject the flag without --prune", flag, err)
		}
	}
	if got := deleted(); len(got) != 0 {
		t.Errorf("deleted = %v, want nothing deleted", got)
	}
}

// TestRevisionsPruneRejectsANegativeKeep checks that a negative keep count is not clamped to 0.
// Clamping it would let a CI job that miscomputed the number delete everything unprotected.
func TestRevisionsPruneRejectsANegativeKeep(t *testing.T) {
	deleted := startPruneAPI(t)

	_, _, err := executeRoot(t, "revisions", "my-svc", "--prune", "--keep", "-1", "--auto-approve",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("revisions --keep -1 error = %v, want it rejected", err)
	}
	if got := deleted(); len(got) != 0 {
		t.Errorf("deleted = %v, want nothing deleted", got)
	}
}

// TestRevisionsPruneJSONWithNothingToDo checks that the JSON output is not empty even on a day
// with nothing to prune, so that a usage like `| jq 'length'` does not break on just that day.
func TestRevisionsPruneJSONWithNothingToDo(t *testing.T) {
	startPruneAPI(t)

	stdout, stderr, err := executeRoot(t, "revisions", "my-svc", "--prune", "--keep", "100",
		"--dry-run", "--format", "json",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("revisions --prune error = %v", err)
	}
	if strings.TrimSpace(stdout) != "[]" {
		t.Errorf("stdout = %q, want an empty JSON array", stdout)
	}
	if !strings.Contains(stderr, "Nothing to prune") {
		t.Errorf("stderr = %q, want the status line", stderr)
	}
}
