package cloudrun

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	run "google.golang.org/api/run/v1"
)

const validWorkerPoolManifest = `apiVersion: run.googleapis.com/v1
kind: WorkerPool
metadata:
  name: my-pool
spec:
  template:
    spec:
      containers:
      - image: gcr.io/project/worker:v1
`

const (
	workerPoolsPath   = "/apis/run.googleapis.com/v1/namespaces/test-project/workerpools"
	myWorkerPoolPath  = workerPoolsPath + "/my-pool"
	workerPoolVersion = "pool-rv-1"
)

// liveWorkerPool is the live worker pool the fake API returns. It carries what the real API adds
// (checked against it): server-managed metadata, the instance count the server defaults to, the
// generated revision name in the status only, and the defaults filled in on create.
func liveWorkerPool(image string) *run.WorkerPool {
	return &run.WorkerPool{
		ApiVersion: "run.googleapis.com/v1",
		Kind:       "WorkerPool",
		Metadata: &run.ObjectMeta{
			Name:            "my-pool",
			Namespace:       "123456",
			Uid:             "uid-1",
			ResourceVersion: workerPoolVersion,
			Generation:      2,
			Labels:          map[string]string{"cloud.googleapis.com/location": testRegion},
			Annotations: map[string]string{
				annotationScalingMode:              "manual",
				annotationManualInstanceCount:      "1",
				"run.googleapis.com/operation-id":  "op-1",
				"serving.knative.dev/creator":      "someone@example.com",
				"serving.knative.dev/lastModifier": "someone@example.com",
			},
		},
		Spec: &run.WorkerPoolSpec{
			Template: &run.RevisionTemplate{Spec: &run.RevisionSpec{
				Containers: []*run.Container{{Image: image}},
			}},
			InstanceSplits: []*run.InstanceSplit{{LatestRevision: true, Percent: 100}},
		},
		Status: &run.WorkerPoolStatus{
			ObservedGeneration:        2,
			LatestCreatedRevisionName: "my-pool-00002-abc",
			LatestReadyRevisionName:   "my-pool-00002-abc",
			InstanceSplits:            []*run.InstanceSplit{{RevisionName: "my-pool-00002-abc", Percent: 100}},
			Conditions:                []*run.GoogleCloudRunV1Condition{{Type: "Ready", Status: "True"}},
		},
	}
}

// withScaling adds instance-count annotations to validWorkerPoolManifest; an empty value leaves
// that annotation out.
func withScaling(mode, count string) string {
	var lines []string
	if mode != "" {
		lines = append(lines, "    run.googleapis.com/scalingMode: "+mode)
	}
	if count != "" {
		lines = append(lines, `    run.googleapis.com/manualInstanceCount: "`+count+`"`)
	}
	if len(lines) == 0 {
		return validWorkerPoolManifest
	}
	return strings.Replace(validWorkerPoolManifest, "  name: my-pool\n",
		"  name: my-pool\n  annotations:\n"+strings.Join(lines, "\n")+"\n", 1)
}

func TestValidateWorkerPool(t *testing.T) {
	for _, ok := range []string{
		validWorkerPoolManifest,
		withScaling("manual", "3"),
		withScaling("manual", "0"),
	} {
		if err := Validate(KindWorkerPool, []byte(ok), "my-pool"); err != nil {
			t.Errorf("Validate(%s) error = %v", ok, err)
		}
	}

	tests := []struct {
		name     string
		manifest string
		want     string
	}{
		{
			name:     "name mismatch",
			manifest: validWorkerPoolManifest,
			want:     `does not match worker pool argument "other"`,
		},
		{
			name:     "a service's apiVersion",
			manifest: strings.Replace(validWorkerPoolManifest, "run.googleapis.com/v1", "serving.knative.dev/v1", 1),
			want:     `apiVersion must be "run.googleapis.com/v1"`,
		},
		{
			// The API sets the count to 0 for this, stopping every instance.
			name:     "manual scaling without a count",
			manifest: withScaling("manual", ""),
			want:     "sets the worker pool to 0 instances",
		},
		{
			// The API rejects this with a 400.
			name:     "a count without manual scaling",
			manifest: withScaling("", "2"),
			want:     "needs run.googleapis.com/scalingMode: manual",
		},
		{
			name:     "a count that is not a number",
			manifest: withScaling("manual", "two"),
			want:     "must be a whole number of 0 or more",
		},
		{
			name:     "a negative count",
			manifest: withScaling("manual", "-1"),
			want:     "must be a whole number of 0 or more",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := "my-pool"
			if tt.name == "name mismatch" {
				name = "other"
			}
			err := Validate(KindWorkerPool, []byte(tt.manifest), name)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestPlanWorkerPoolCreate(t *testing.T) {
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if r.Method == http.MethodPost {
			return http.StatusOK, liveWorkerPool("gcr.io/project/worker:v1")
		}
		return http.StatusNotFound, googleAPIError(404, "not found")
	})

	plan, err := c.Plan(context.Background(), KindWorkerPool, "my-pool", []byte(validWorkerPoolManifest), PlanOptions{})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if !plan.Create || !strings.Contains(plan.Diff, "+kind: WorkerPool") || plan.Noun() != "worker pool" {
		t.Errorf("plan = %+v, want a create showing the whole worker pool as added", plan)
	}
	if got := lastRequest(t, api, http.MethodGet).Path; got != myWorkerPoolPath {
		t.Errorf("GET path = %q, want %q", got, myWorkerPoolPath)
	}

	generation, err := plan.Apply(context.Background(), false)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if generation != 2 {
		t.Errorf("Apply() = %d, want the applied worker pool's generation", generation)
	}
	post := lastRequest(t, api, http.MethodPost)
	if post.Path != workerPoolsPath || strings.Contains(post.Query, "dryRun") {
		t.Errorf("create = %s?%s, want %s with no dryRun", post.Path, post.Query, workerPoolsPath)
	}
	if !strings.Contains(string(post.Body), `"namespace":"test-project"`) {
		t.Errorf("create body = %s, want the namespace set to the project", post.Body)
	}
}

// TestPlanWorkerPoolUpdateIsACompareAndSwap checks that a worker pool is replaced against the
// resourceVersion the diff was computed from, as a service is.
func TestPlanWorkerPoolUpdateIsACompareAndSwap(t *testing.T) {
	c, api := newTestClient(t, func(*http.Request) (int, interface{}) {
		return http.StatusOK, liveWorkerPool("gcr.io/project/worker:v1")
	})

	manifest := strings.Replace(validWorkerPoolManifest, "worker:v1", "worker:v2", 1)
	plan, err := c.Plan(context.Background(), KindWorkerPool, "my-pool", []byte(manifest), PlanOptions{})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if plan.Create || !strings.Contains(plan.Diff, "+      - image: gcr.io/project/worker:v2") {
		t.Errorf("plan = %+v, want an update showing the image change", plan)
	}
	if _, err := plan.Apply(context.Background(), false); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	put := lastRequest(t, api, http.MethodPut)
	if put.Path != myWorkerPoolPath {
		t.Errorf("replace path = %q, want %q", put.Path, myWorkerPoolPath)
	}
	var sent run.WorkerPool
	if err := json.Unmarshal(put.Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Metadata == nil || sent.Metadata.ResourceVersion != workerPoolVersion {
		t.Errorf("sent resourceVersion = %+v, want %q", sent.Metadata, workerPoolVersion)
	}
}

// TestWorkerPoolDiffIgnoresServerManagedFields checks that a manifest written the way init writes
// one diffs clean against the live worker pool: the metadata the server manages and a revision
// name the manifest does not pin are not differences.
func TestWorkerPoolDiffIgnoresServerManagedFields(t *testing.T) {
	live := liveWorkerPool("gcr.io/project/worker:v1")
	live.Spec.Template.Metadata = &run.ObjectMeta{Name: "my-pool-pinned"}
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) { return http.StatusOK, live })

	manifest := withScaling("manual", "1") + "  instanceSplits:\n  - latestRevision: true\n    percent: 100\n"
	diff, err := c.CompareManifest(context.Background(), KindWorkerPool, "my-pool", []byte(manifest),
		"pool.yaml", PlanOptions{})
	if err != nil {
		t.Fatalf("CompareManifest() error = %v", err)
	}
	if diff != "" {
		t.Errorf("diff = %s, want none", diff)
	}
}

// TestWorkerPoolLiveManifestDropsThePinnedRevision checks that init does not scaffold a revision
// name, which would make the second template change fail with a 409.
func TestWorkerPoolLiveManifestDropsThePinnedRevision(t *testing.T) {
	live := liveWorkerPool("gcr.io/project/worker:v1")
	live.Spec.Template.Metadata = &run.ObjectMeta{Name: "my-pool-pinned"}
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) { return http.StatusOK, live })

	out, err := c.LiveManifest(context.Background(), KindWorkerPool, "my-pool")
	if err != nil {
		t.Fatalf("LiveManifest() error = %v", err)
	}
	text := string(out)
	for _, unwanted := range []string{"my-pool-pinned", "resourceVersion", "operation-id", "status:"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("manifest contains %q:\n%s", unwanted, text)
		}
	}
	for _, want := range []string{"kind: WorkerPool", "run.googleapis.com/manualInstanceCount", "instanceSplits"} {
		if !strings.Contains(text, want) {
			t.Errorf("manifest lacks %q:\n%s", want, text)
		}
	}
	if err := Validate(KindWorkerPool, out, "my-pool"); err != nil {
		t.Errorf("the scaffold does not validate: %v", err)
	}
	if live.Spec.Template.Metadata.Name != "my-pool-pinned" {
		t.Error("LiveManifest modified the live definition")
	}
}

func TestWorkerPoolRevisionName(t *testing.T) {
	pinned := strings.Replace(validWorkerPoolManifest, "    spec:\n", "    metadata:\n      name: my-pool-v1\n    spec:\n", 1)
	for manifest, want := range map[string]string{validWorkerPoolManifest: "", pinned: "my-pool-v1"} {
		got, err := RevisionName(KindWorkerPool, []byte(manifest))
		if err != nil || got != want {
			t.Errorf("RevisionName() = %q, %v; want %q", got, err, want)
		}
	}
}

func TestWorkerPoolDeleteAndWaitDeleted(t *testing.T) {
	gets := 0
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		switch r.Method {
		case http.MethodDelete:
			return http.StatusOK, map[string]any{"status": "Success"}
		default:
			// Deletion is asynchronous: still readable once, then gone.
			gets++
			if gets == 1 {
				return http.StatusOK, liveWorkerPool("gcr.io/project/worker:v1")
			}
			return http.StatusNotFound, googleAPIError(404, "not found")
		}
	})
	if err := c.Delete(context.Background(), KindWorkerPool, "my-pool", false); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if del := lastRequest(t, api, http.MethodDelete); del.Path != myWorkerPoolPath || strings.Contains(del.Query, "dryRun") {
		t.Errorf("DELETE = %s?%s, want %s with no dryRun", del.Path, del.Query, myWorkerPoolPath)
	}
	if err := c.WaitDeleted(context.Background(), KindWorkerPool, "my-pool", fastWait(0)); err != nil {
		t.Fatalf("WaitDeleted() error = %v", err)
	}
	if gets != 2 {
		t.Errorf("polled %d times, want until the 404", gets)
	}
}

func TestWorkerPoolWait(t *testing.T) {
	live := liveWorkerPool("gcr.io/project/worker:v1")
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) { return http.StatusOK, live })
	status, err := c.Wait(context.Background(), KindWorkerPool, "my-pool", fastWait(0))
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if status.Service != "my-pool" || status.LatestReadyRevision != "my-pool-00002-abc" {
		t.Errorf("status = %+v", status)
	}
}

func TestWorkerPoolStatus(t *testing.T) {
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) {
		return http.StatusOK, liveWorkerPool("gcr.io/project/worker:v1")
	})
	status, err := c.GetWorkerPoolStatus(context.Background(), "my-pool")
	if err != nil {
		t.Fatalf("GetWorkerPoolStatus() error = %v", err)
	}
	text := status.Text()
	for _, want := range []string{
		"Worker pool:     my-pool\n",
		"Ready:           True\n",
		"Instances:       1 (manual)\n",
		"Latest ready:    my-pool-00002-abc\n",
		"Generation:      2 (observed 2)\n",
		"Instance split:\n  100%  my-pool-00002-abc\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Text() lacks %q:\n%s", want, text)
		}
	}

	b, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"workerPool":"my-pool"`, `"instanceCount":1`, `"scalingMode":"manual"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON lacks %s: %s", want, b)
		}
	}
}

// TestWorkerPoolStatusShowsAStoppedPool checks that a count of 0 is shown rather than dropped as
// an empty value: 0 is how a worker pool is stopped.
func TestWorkerPoolStatusShowsAStoppedPool(t *testing.T) {
	live := liveWorkerPool("x")
	live.Metadata.Annotations[annotationManualInstanceCount] = "0"
	status := newWorkerPoolStatus(live)
	if !strings.Contains(status.Text(), "Instances:       0 (manual, stopped)\n") {
		t.Errorf("Text() = %s", status.Text())
	}
	b, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"instanceCount":0`) {
		t.Errorf("JSON = %s, want instanceCount 0 kept", b)
	}

	// Nil-safe, and a worker pool that does not say how many instances it runs shows no count.
	for _, pool := range []*run.WorkerPool{nil, {}, {Metadata: &run.ObjectMeta{Name: "p"}}} {
		s := newWorkerPoolStatus(pool)
		if s.InstanceCount != nil || strings.Contains(s.Text(), "Instances:") {
			t.Errorf("newWorkerPoolStatus(%+v) = %+v, text %q", pool, s, s.Text())
		}
	}
}
