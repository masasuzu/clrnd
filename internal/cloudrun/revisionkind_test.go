package cloudrun

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	run "google.golang.org/api/run/v1"
)

// poolWithRevisions is a live worker pool after three deploys: revision 3 is the latest created
// and ready, and spec.instanceSplits follows the latest revision.
func poolWithRevisions() *run.WorkerPool {
	pool := liveWorkerPool("img")
	pool.Status.LatestCreatedRevisionName = "my-pool-00003-ccc"
	pool.Status.LatestReadyRevisionName = "my-pool-00003-ccc"
	pool.Status.InstanceSplits = []*run.InstanceSplit{{RevisionName: "my-pool-00003-ccc", Percent: 100}}
	return pool
}

func poolRevisionList() *run.ListRevisionsResponse {
	return &run.ListRevisionsResponse{Items: []*run.Revision{
		revision("my-pool-00001-aaa", "img:v1", "2026-08-20T10:00:00Z", conditionTrue, "Retired"),
		revision("my-pool-00003-ccc", "img:v3", "2026-08-22T10:00:00Z", conditionTrue, ""),
		revision("my-pool-00002-bbb", "img:v2", "2026-08-21T10:00:00Z", conditionTrue, "Retired"),
	}}
}

// workerPoolRevisionsAPI serves pool and its revisions, and records the label selector and every
// write.
func workerPoolRevisionsAPI(t *testing.T, pool *run.WorkerPool) (*Client, *fakeAPI, *string) {
	t.Helper()
	selector := new(string)
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revisions"):
			*selector = r.URL.Query().Get("labelSelector")
			return http.StatusOK, poolRevisionList()
		case r.Method == http.MethodPut:
			return http.StatusOK, pool
		default:
			return http.StatusOK, pool
		}
	})
	return c, api, selector
}

func TestListWorkerPoolRevisions(t *testing.T) {
	c, _, selector := workerPoolRevisionsAPI(t, poolWithRevisions())
	got, err := c.ListKindRevisions(context.Background(), KindWorkerPool, "my-pool")
	if err != nil {
		t.Fatalf("ListKindRevisions() error = %v", err)
	}
	if *selector != "run.googleapis.com/workerPool=my-pool" {
		t.Errorf("labelSelector = %q, want the worker pool's label", *selector)
	}
	if len(got) != 3 || got[0].Name != "my-pool-00003-ccc" || got[0].Percent != 100 || got[1].Percent != 0 {
		t.Fatalf("revisions = %+v, want newest first with the instance split joined in", got)
	}
	text := got.InstancesText()
	if !strings.HasPrefix(text, "REVISION") || !strings.Contains(text, "INSTANCES") || strings.Contains(text, "TAGS") {
		t.Errorf("InstancesText() = %s", text)
	}
	// The serving revision is protected; the older ones are not.
	if pruned := SelectPrunableRevisions(got, 0); len(pruned) != 2 || pruned[0].Name != "my-pool-00002-bbb" {
		t.Errorf("SelectPrunableRevisions(keep=0) = %+v, want the two retired revisions", pruned)
	}
}

// TestWorkerPoolPruneAfterRollback checks the state a rollback leaves: the latest created revision
// runs nothing, yet Cloud Run refuses to delete it, so it stays protected.
func TestWorkerPoolPruneAfterRollback(t *testing.T) {
	pool := poolWithRevisions()
	pool.Spec.InstanceSplits = []*run.InstanceSplit{{RevisionName: "my-pool-00002-bbb", Percent: 100}}
	pool.Status.InstanceSplits = []*run.InstanceSplit{{RevisionName: "my-pool-00002-bbb", Percent: 100}}
	c, _, _ := workerPoolRevisionsAPI(t, pool)
	got, err := c.ListKindRevisions(context.Background(), KindWorkerPool, "my-pool")
	if err != nil {
		t.Fatal(err)
	}
	pruned := SelectPrunableRevisions(got, 0)
	if len(pruned) != 1 || pruned[0].Name != "my-pool-00001-aaa" {
		t.Errorf("SelectPrunableRevisions(keep=0) = %+v, want only the oldest revision", pruned)
	}
}

func TestPlanWorkerPoolRollback(t *testing.T) {
	c, api, _ := workerPoolRevisionsAPI(t, poolWithRevisions())
	plan, chosen, err := c.PlanRollback(context.Background(), KindWorkerPool, "my-pool", "")
	if err != nil {
		t.Fatalf("PlanRollback() error = %v", err)
	}
	if chosen.Name != "my-pool-00002-bbb" {
		t.Errorf("chosen = %q, want the revision before the one running the instances", chosen.Name)
	}
	if !strings.Contains(plan.Diff, "-  - latestRevision: true") ||
		!strings.Contains(plan.Diff, "+    revisionName: my-pool-00002-bbb") {
		t.Errorf("diff does not show the split moving:\n%s", plan.Diff)
	}
	if _, err := plan.Apply(context.Background(), false); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	var sent run.WorkerPool
	if err := json.Unmarshal(lastRequest(t, api, http.MethodPut).Body, &sent); err != nil {
		t.Fatal(err)
	}
	splits := sent.Spec.InstanceSplits
	if len(splits) != 1 || splits[0].RevisionName != "my-pool-00002-bbb" || splits[0].Percent != 100 {
		t.Errorf("sent instanceSplits = %+v, want every instance on revision 2", splits)
	}
	if sent.Metadata.ResourceVersion != workerPoolVersion {
		t.Errorf("sent resourceVersion = %q, want the version of the read it edits", sent.Metadata.ResourceVersion)
	}
	if sent.Spec.Template == nil || len(sent.Spec.Template.Spec.Containers) != 1 {
		t.Error("the rollback changed the template; only the split may change")
	}

	if _, _, err := c.PlanRollback(context.Background(), KindWorkerPool, "my-pool", "my-pool-00009-zzz"); err == nil ||
		!strings.Contains(err.Error(), "does not belong to this worker pool") {
		t.Errorf("PlanRollback(unknown revision) error = %v", err)
	}
}

// TestWorkerPoolRollbackRefusesAnUnfinishedRollout checks the worker pool wording of the refusal
// rollback gives when the newest revision failed and runs nothing.
func TestWorkerPoolRollbackRefusesAnUnfinishedRollout(t *testing.T) {
	revisions := Revisions{
		{Name: "my-pool-00003-ccc", Ready: conditionFalse, Reason: "ContainerMissing"},
		{Name: "my-pool-00002-bbb", Ready: conditionTrue, Percent: 100},
	}
	_, err := selectRollbackRevision(revisions, "", workerPoolSplitWords)
	if err == nil || !strings.Contains(err.Error(), "runs no instances; the instances are still on") {
		t.Errorf("error = %v", err)
	}
}

func TestJobsHaveNoRevisions(t *testing.T) {
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) {
		t.Error("a kind without revisions made an API call")
		return http.StatusInternalServerError, nil
	})
	if _, err := c.ListKindRevisions(context.Background(), KindJob, "my-job"); err == nil ||
		!strings.Contains(err.Error(), "a job has no revisions") {
		t.Errorf("ListKindRevisions(job) error = %v", err)
	}
	if _, _, err := c.PlanRollback(context.Background(), KindJob, "my-job", ""); err == nil {
		t.Error("PlanRollback(job) succeeded")
	}
}
