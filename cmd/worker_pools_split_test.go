package cmd

import (
	"strings"
	"testing"
)

// lastPut returns the last write among the recorded calls, or "" when there was none.
func lastPut(calls []string) string {
	var put string
	for _, c := range calls {
		if strings.HasPrefix(c, "PUT ") {
			put = c
		}
	}
	return put
}

func TestWorkerPoolsInstanceSplitEndToEnd(t *testing.T) {
	calls := workerPoolRevisionsAPI(t)
	stdout, _, err := executeRoot(t, "worker-pools", "instance-split", "my-pool",
		"--to", "my-pool-00004-xyz", "--percent", "25", "--auto-approve", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("worker-pools instance-split error = %v", err)
	}
	if !strings.Contains(stdout, "revisionName: my-pool-00004-xyz") {
		t.Errorf("worker-pools instance-split stdout = %q, want the split in the diff", stdout)
	}
	put := lastPut(calls())
	if !strings.Contains(put, `"instanceSplits":[{"percent":25,"revisionName":"my-pool-00004-xyz"},`+
		`{"percent":75,"revisionName":"my-pool-00005-abc"}]`) || !strings.Contains(put, `"resourceVersion":"rv-5"`) {
		t.Errorf("replace = %s, want 25%% on the named revision and the rest where it runs now, "+
			"against the read version", put)
	}
}

// TestWorkerPoolsInstanceSplitValidatesTheFlagsFirst checks that a missing target is reported in
// the worker pool's terms before target resolution and authentication (no --project is passed).
func TestWorkerPoolsInstanceSplitValidatesTheFlagsFirst(t *testing.T) {
	_, _, err := executeRoot(t, "worker-pools", "instance-split", "my-pool", "--percent", "50")
	if err == nil || !strings.Contains(err.Error(), "no revision to run the instances on: pass --to or --to-latest") {
		t.Errorf("worker-pools instance-split error = %v, want the flag error", err)
	}
}

func TestWorkerPoolsInstanceSplitHasNoTrafficWording(t *testing.T) {
	stdout, stderr, err := executeRoot(t, "worker-pools", "instance-split", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if help := stdout + stderr; strings.Contains(help, "traffic") {
		t.Errorf("worker-pools instance-split --help talks about traffic:\n%s", help)
	}
}

// TestWorkerPoolsDeployNoPromotePinsTheCurrentSplit checks that --no-promote sends the split the
// worker pool runs now, pinned by name, in place of the one the manifest writes, and says so.
func TestWorkerPoolsDeployNoPromotePinsTheCurrentSplit(t *testing.T) {
	calls := workerPoolRevisionsAPI(t)
	manifest := writeManifest(t, localWorkerPoolManifest+"  instanceSplits:\n  - latestRevision: true\n    percent: 100\n")
	_, stderr, err := executeRoot(t, "worker-pools", "deploy", "my-pool", manifest, "--no-promote",
		"--no-server-defaults", "--auto-approve", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("worker-pools deploy --no-promote error = %v", err)
	}
	if !strings.Contains(stderr, "warning: --no-promote replaces the spec.instanceSplits written in the manifest") {
		t.Errorf("stderr = %q, want a warning that the manifest's split is replaced", stderr)
	}
	put := lastPut(calls())
	if !strings.Contains(put, `"instanceSplits":[{"percent":100,"revisionName":"my-pool-00005-abc"}]`) ||
		!strings.Contains(put, "gcr.io/project/worker:new") {
		t.Errorf("replace = %s, want the new template with the split pinned to the running revision", put)
	}
}

// TestNoPromoteIsAWorkerPoolFlag checks that deploy offers each kind its own flag for keeping the
// split, and none to a kind without revisions.
func TestNoPromoteIsAWorkerPoolFlag(t *testing.T) {
	manifest := writeManifest(t, localManifest)
	for _, args := range [][]string{
		{"deploy", "my-svc", manifest, "--no-promote"},
		{"jobs", "deploy", "my-job", manifest, "--no-promote"},
	} {
		if _, _, err := executeRoot(t, args...); err == nil || !strings.Contains(err.Error(), "unknown flag: --no-promote") {
			t.Errorf("%v error = %v, want an unknown flag", args, err)
		}
	}
}
