package cloudrun

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	run "google.golang.org/api/run/v1"
)

// poolAfterNoPromote is a live worker pool just after deploy --no-promote: revision 3 is the
// latest created and runs nothing, revision 2 runs every instance and is still the latest ready.
func poolAfterNoPromote(count string) *run.WorkerPool {
	pool := poolWithRevisions()
	pool.Metadata.Annotations[annotationManualInstanceCount] = count
	pool.Spec.InstanceSplits = []*run.InstanceSplit{{RevisionName: "my-pool-00002-bbb", Percent: 100}}
	pool.Status.LatestReadyRevisionName = "my-pool-00002-bbb"
	pool.Status.InstanceSplits = []*run.InstanceSplit{{RevisionName: "my-pool-00002-bbb", Percent: 100}}
	return pool
}

func assertInstanceSplits(t *testing.T, got *run.WorkerPool, want []*run.InstanceSplit) {
	t.Helper()
	splits := got.Spec.InstanceSplits
	if len(splits) != len(want) {
		t.Fatalf("instanceSplits = %+v, want %+v", splits, want)
	}
	for i := range want {
		if g, w := splits[i], want[i]; g.RevisionName != w.RevisionName || g.LatestRevision != w.LatestRevision || g.Percent != w.Percent {
			t.Errorf("instanceSplits[%d] = %+v, want %+v", i, splits[i], want[i])
		}
	}
}

// TestShiftInstanceSplitSplitsAgainstTheLargestShare checks the canary shape: the share goes to the
// named revision and the rest stays on the revision running the most.
func TestShiftInstanceSplitSplitsAgainstTheLargestShare(t *testing.T) {
	live := poolAfterNoPromote("2")
	got, err := shiftInstanceSplit(live, SplitRequest{Revision: "my-pool-00003-ccc", Percent: 10})
	if err != nil {
		t.Fatalf("shiftInstanceSplit() error = %v", err)
	}
	assertInstanceSplits(t, got, []*run.InstanceSplit{
		{RevisionName: "my-pool-00003-ccc", Percent: 10},
		{RevisionName: "my-pool-00002-bbb", Percent: 90},
	})
	if got.Spec.Template != live.Spec.Template {
		t.Error("the template was replaced; a split change must not create a revision")
	}
	if len(live.Spec.InstanceSplits) != 1 || live.Spec.InstanceSplits[0].RevisionName != "my-pool-00002-bbb" {
		t.Errorf("the live worker pool was modified: %+v", live.Spec.InstanceSplits)
	}
}

// TestShiftInstanceSplitToLatestAfterNoPromote checks that --to-latest is measured against the
// newest created revision, which is what latestRevision resolves to for a worker pool. Measured
// against latestReadyRevisionName, which still names the revision running every instance, this
// canary would be refused as "already running 100%".
func TestShiftInstanceSplitToLatestAfterNoPromote(t *testing.T) {
	got, err := shiftInstanceSplit(poolAfterNoPromote("2"), SplitRequest{Latest: true, Percent: 10})
	if err != nil {
		t.Fatalf("shiftInstanceSplit() error = %v", err)
	}
	assertInstanceSplits(t, got, []*run.InstanceSplit{
		{LatestRevision: true, Percent: 10},
		{RevisionName: "my-pool-00002-bbb", Percent: 90},
	})
}

// TestShiftInstanceSplitRefusesToDemoteTheRunningRevision checks the worker pool wording of the
// refusal traffic gives when the target already runs the largest share.
func TestShiftInstanceSplitRefusesToDemoteTheRunningRevision(t *testing.T) {
	_, err := shiftInstanceSplit(poolAfterNoPromote("2"), SplitRequest{Revision: "my-pool-00002-bbb", Percent: 10})
	if err == nil || !strings.Contains(err.Error(), "is already running 100% of the instances") {
		t.Errorf("shiftInstanceSplit() error = %v", err)
	}
}

// TestShiftInstanceSplitNeedsAnInstancePerRevision checks that a split between two revisions is
// refused for a worker pool running a single instance, which the API rejects, and allowed when it
// runs more, when it is stopped, or when everything goes to one revision.
func TestShiftInstanceSplitNeedsAnInstancePerRevision(t *testing.T) {
	_, err := shiftInstanceSplit(poolAfterNoPromote("1"), SplitRequest{Revision: "my-pool-00003-ccc", Percent: 50})
	if err == nil || !strings.Contains(err.Error(), "runs 1 instance, and a split between two revisions needs one for each") {
		t.Errorf("shiftInstanceSplit(1 instance, 50%%) error = %v", err)
	}
	for _, tt := range []struct {
		count   string
		percent int64
	}{{"2", 50}, {"0", 50}, {"1", 100}} {
		if _, err := shiftInstanceSplit(poolAfterNoPromote(tt.count), SplitRequest{Revision: "my-pool-00003-ccc", Percent: tt.percent}); err != nil {
			t.Errorf("shiftInstanceSplit(%s instances, %d%%) error = %v", tt.count, tt.percent, err)
		}
	}
}

// TestPlanWorkerPoolSplit checks instance-split end to end against the fake API: the revision is
// checked against the worker pool's own, and the write is a compare-and-swap that changes only the
// split.
func TestPlanWorkerPoolSplit(t *testing.T) {
	c, api, _ := workerPoolRevisionsAPI(t, poolAfterNoPromote("2"))
	plan, target, err := c.PlanSplit(context.Background(), KindWorkerPool, "my-pool",
		SplitRequest{Revision: "my-pool-00003-ccc", Percent: 50})
	if err != nil {
		t.Fatalf("PlanSplit() error = %v", err)
	}
	if target == nil || target.Name != "my-pool-00003-ccc" {
		t.Errorf("target = %+v, want the named revision", target)
	}
	if !strings.Contains(plan.Diff, "+    revisionName: my-pool-00003-ccc") {
		t.Errorf("diff does not show the split moving:\n%s", plan.Diff)
	}
	if _, err := plan.Apply(context.Background(), false); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	var sent run.WorkerPool
	if err := json.Unmarshal(lastRequest(t, api, http.MethodPut).Body, &sent); err != nil {
		t.Fatal(err)
	}
	assertInstanceSplits(t, &sent, []*run.InstanceSplit{
		{RevisionName: "my-pool-00003-ccc", Percent: 50},
		{RevisionName: "my-pool-00002-bbb", Percent: 50},
	})
	if sent.Metadata.ResourceVersion != workerPoolVersion {
		t.Errorf("sent resourceVersion = %q, want the version of the read it edits", sent.Metadata.ResourceVersion)
	}

	if _, _, err := c.PlanSplit(context.Background(), KindWorkerPool, "my-pool",
		SplitRequest{Revision: "my-pool-00009-zzz", Percent: 100}); err == nil ||
		!strings.Contains(err.Error(), "does not belong to this worker pool") {
		t.Errorf("PlanSplit(unknown revision) error = %v", err)
	}
	if _, _, err := c.PlanSplit(context.Background(), KindJob, "my-job", SplitRequest{Latest: true, Percent: 100}); err == nil ||
		!strings.Contains(err.Error(), "a job has no revisions") {
		t.Errorf("PlanSplit(job) error = %v", err)
	}
}

// TestWorkerPoolDeployKeepsTheInstanceSplit checks deploy --no-promote: the manifest's split is
// replaced with the one running now, pinned by name, so the new revision gets no instances.
// Revisions at 0% are left out.
func TestWorkerPoolDeployKeepsTheInstanceSplit(t *testing.T) {
	live := liveWorkerPool("gcr.io/project/worker:v1")
	live.Status.InstanceSplits = []*run.InstanceSplit{
		{RevisionName: "my-pool-00002-abc", Percent: 90},
		{RevisionName: "my-pool-00001-xyz", Percent: 10},
		{RevisionName: "my-pool-00000-old", Percent: 0},
	}
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) { return http.StatusOK, live })
	manifest := strings.Replace(validWorkerPoolManifest, "worker:v1", "worker:v2", 1) +
		"  instanceSplits:\n  - latestRevision: true\n    percent: 100\n"
	plan, err := c.Plan(context.Background(), KindWorkerPool, "my-pool", []byte(manifest), PlanOptions{KeepSplit: true})
	if err != nil {
		t.Fatalf("Plan(KeepSplit) error = %v", err)
	}
	assertInstanceSplits(t, plan.desired.(*run.WorkerPool), []*run.InstanceSplit{
		{RevisionName: "my-pool-00002-abc", Percent: 90},
		{RevisionName: "my-pool-00001-xyz", Percent: 10},
	})
	if !strings.Contains(plan.Diff, "+    revisionName: my-pool-00002-abc") {
		t.Errorf("diff does not show the pinned split:\n%s", plan.Diff)
	}
}

// TestWorkerPoolDeployCannotKeepASplitOnCreate checks that --no-promote is refused for a worker
// pool that does not exist yet: its first revision has to run the instances.
func TestWorkerPoolDeployCannotKeepASplitOnCreate(t *testing.T) {
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) {
		return http.StatusNotFound, googleAPIError(404, "not found")
	})
	_, err := c.Plan(context.Background(), KindWorkerPool, "my-pool", []byte(validWorkerPoolManifest), PlanOptions{KeepSplit: true})
	if err == nil || !strings.Contains(err.Error(), "does not exist yet") {
		t.Errorf("Plan(KeepSplit) on create error = %v", err)
	}
}
