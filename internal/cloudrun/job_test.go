package cloudrun

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	run "google.golang.org/api/run/v1"
)

// validJobManifest is a minimal job manifest that passes Validate.
const validJobManifest = `apiVersion: run.googleapis.com/v1
kind: Job
metadata:
  name: my-job
spec:
  template:
    spec:
      template:
        spec:
          containers:
          - image: gcr.io/project/job:v1
`

const (
	jobsPath   = "/apis/run.googleapis.com/v1/namespaces/test-project/jobs"
	myJobPath  = jobsPath + "/my-job"
	jobVersion = "job-rv-1"
)

// liveJob is the live job the fake API returns. It carries what the real API adds (checked
// against it): server-managed metadata, the labels and annotations that record who changed the
// job and when, and the defaults filled in on create.
func liveJob(image string) *run.Job {
	return &run.Job{
		ApiVersion: "run.googleapis.com/v1",
		Kind:       "Job",
		Metadata: &run.ObjectMeta{
			Name:            "my-job",
			Namespace:       "123456",
			Uid:             "uid-1",
			ResourceVersion: jobVersion,
			Generation:      3,
			Labels: map[string]string{
				"cloud.googleapis.com/location":      testRegion,
				"run.googleapis.com/lastUpdatedTime": "2026-01-02T03:04:05.000000Z",
			},
			Annotations: map[string]string{
				"run.googleapis.com/creator":      "someone@example.com",
				"run.googleapis.com/lastModifier": "someone@example.com",
				"run.googleapis.com/operation-id": "op-1",
			},
		},
		Spec: &run.JobSpec{
			Template: &run.ExecutionTemplateSpec{
				Spec: &run.ExecutionSpec{
					Template: &run.TaskTemplateSpec{
						Spec: &run.TaskSpec{Containers: []*run.Container{{Image: image}}},
					},
				},
			},
		},
		Status: &run.JobStatus{
			ObservedGeneration: 3,
			Conditions:         []*run.GoogleCloudRunV1Condition{{Type: "Ready", Status: "True"}},
		},
	}
}

func TestJobWorkload(t *testing.T) {
	t.Run("nil-safe", func(t *testing.T) {
		for _, j := range []*run.Job{nil, {}, {Spec: &run.JobSpec{Template: &run.ExecutionTemplateSpec{}}}} {
			w := jobWorkload(j)
			if w.Containers != nil || w.Annotations != nil || w.ServiceAccountName != "" {
				t.Errorf("jobWorkload(%+v) = %+v, want only the path set", j, w)
			}
			if w.Path != "spec.template.spec.template.spec" {
				t.Errorf("Path = %q", w.Path)
			}
		}
	})

	t.Run("reads the task template and the execution annotations", func(t *testing.T) {
		job := &run.Job{Spec: &run.JobSpec{Template: &run.ExecutionTemplateSpec{
			Metadata: &run.ObjectMeta{Annotations: map[string]string{vpcConnectorAnnotation: "conn"}},
			Spec: &run.ExecutionSpec{Template: &run.TaskTemplateSpec{Spec: &run.TaskSpec{
				Containers:         []*run.Container{{Image: "x"}},
				Volumes:            []*run.Volume{{Name: "v"}},
				ServiceAccountName: "sa@p.iam.gserviceaccount.com",
			}}},
		}}}
		w := jobWorkload(job)
		if len(w.Containers) != 1 || w.Containers[0].Image != "x" || len(w.Volumes) != 1 ||
			w.ServiceAccountName != "sa@p.iam.gserviceaccount.com" || w.annotation(vpcConnectorAnnotation) != "conn" {
			t.Errorf("jobWorkload() = %+v", w)
		}
	})
}

func TestValidateJob(t *testing.T) {
	if err := Validate(KindJob, []byte(validJobManifest), "my-job"); err != nil {
		t.Fatalf("Validate(valid job) error = %v", err)
	}

	tests := []struct {
		name     string
		manifest string
		job      string
		want     string
	}{
		{
			name:     "name mismatch",
			manifest: validJobManifest,
			job:      "other-job",
			want:     `metadata.name "my-job" does not match job argument "other-job"`,
		},
		{
			name:     "a service's apiVersion",
			manifest: strings.Replace(validJobManifest, "run.googleapis.com/v1", "serving.knative.dev/v1", 1),
			job:      "my-job",
			want:     `apiVersion must be "run.googleapis.com/v1"`,
		},
		{
			name: "no containers",
			manifest: `apiVersion: run.googleapis.com/v1
kind: Job
metadata:
  name: my-job
spec:
  template:
    spec:
      template:
        spec:
          containers: []
`,
			job:  "my-job",
			want: "spec.template.spec.template.spec.containers must define at least one container",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(KindJob, []byte(tt.manifest), tt.job)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// TestManifestOfTheWrongKind checks that a manifest given to the commands for another kind says
// so and where it belongs, instead of failing on the first field the other kind does not have.
func TestManifestOfTheWrongKind(t *testing.T) {
	tests := []struct {
		name     string
		kind     Kind
		manifest string
		want     []string
	}{
		{
			name:     "a job to the service commands",
			kind:     KindService,
			manifest: validJobManifest,
			want:     []string{`kind is "Job"`, "'clrnd jobs'"},
		},
		{
			name:     "a service to the job commands",
			kind:     KindJob,
			manifest: validManifest,
			want:     []string{`kind is "Service"`, "'clrnd deploy'"},
		},
		{
			name:     "a worker pool",
			kind:     KindService,
			manifest: "apiVersion: run.googleapis.com/v1\nkind: WorkerPool\nmetadata:\n  name: my-pool\n",
			want:     []string{`kind is "WorkerPool"`, "does not support"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for name, check := range map[string]func() error{
				"Validate":    func() error { return Validate(tt.kind, []byte(tt.manifest), "x") },
				"CheckSyntax": func() error { return CheckSyntax(tt.kind, []byte(tt.manifest)) },
				"ApplyImageOverrides": func() error {
					_, err := ApplyImageOverrides(tt.kind, []byte(tt.manifest), []string{"img"})
					return err
				},
			} {
				err := check()
				for _, want := range tt.want {
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Errorf("%s() error = %v, want it to contain %q", name, err, want)
					}
				}
			}
		})
	}

	// A kind clrnd has never heard of is left to validate, which already explains it.
	err := Validate(KindService, []byte(strings.Replace(validManifest, "kind: Service", "kind: Deployment", 1)), "my-svc")
	if err == nil || !strings.Contains(err.Error(), `kind must be "Service", got "Deployment"`) {
		t.Errorf("Validate(kind: Deployment) error = %v", err)
	}
}

func TestApplyImageOverridesToAJob(t *testing.T) {
	got, err := ApplyImageOverrides(KindJob, []byte(validJobManifest), []string{"gcr.io/project/job:v2"})
	if err != nil {
		t.Fatalf("ApplyImageOverrides() error = %v", err)
	}
	if err := Validate(KindJob, got, "my-job"); err != nil {
		t.Fatalf("the result is not a valid job: %v", err)
	}
	job, err := jobKind.parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if c := jobWorkload(job).Containers; len(c) != 1 || c[0].Image != "gcr.io/project/job:v2" {
		t.Errorf("containers = %+v, want the task's image replaced", c)
	}
}

func TestPlanJobCreate(t *testing.T) {
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if r.Method == http.MethodPost {
			return http.StatusOK, liveJob("gcr.io/project/job:v1")
		}
		return http.StatusNotFound, googleAPIError(404, "not found")
	})

	plan, err := c.Plan(context.Background(), KindJob, "my-job", []byte(validJobManifest), PlanOptions{})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if !plan.Create || !strings.Contains(plan.Diff, "+kind: Job") {
		t.Errorf("plan = %+v, want a create showing the whole job as added", plan)
	}
	if got := lastRequest(t, api, http.MethodGet).Path; got != myJobPath {
		t.Errorf("GET path = %q, want %q", got, myJobPath)
	}

	generation, err := plan.Apply(context.Background(), false)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if generation != 3 {
		t.Errorf("Apply() = %d, want the applied job's generation", generation)
	}
	post := lastRequest(t, api, http.MethodPost)
	if post.Path != jobsPath || strings.Contains(post.Query, "dryRun") {
		t.Errorf("create = %s?%s, want %s with no dryRun", post.Path, post.Query, jobsPath)
	}
	if !strings.Contains(string(post.Body), `"namespace":"test-project"`) {
		t.Errorf("create body = %s, want the namespace set to the project", post.Body)
	}
}

// TestPlanJobUpdateIsACompareAndSwap checks that a job is replaced against the resourceVersion
// the diff was computed from, as a service is. Without it the write is an unconditional overwrite.
func TestPlanJobUpdateIsACompareAndSwap(t *testing.T) {
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		return http.StatusOK, liveJob("gcr.io/project/job:v1")
	})

	manifest := strings.Replace(validJobManifest, "job:v1", "job:v2", 1)
	plan, err := c.Plan(context.Background(), KindJob, "my-job", []byte(manifest), PlanOptions{})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if plan.Create {
		t.Fatal("plan.Create = true, want an update of the live job")
	}
	if !strings.Contains(plan.Diff, "-          - image: gcr.io/project/job:v1") ||
		!strings.Contains(plan.Diff, "+          - image: gcr.io/project/job:v2") {
		t.Errorf("diff does not show the image change:\n%s", plan.Diff)
	}
	if _, err := plan.Apply(context.Background(), false); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	put := lastRequest(t, api, http.MethodPut)
	if put.Path != myJobPath {
		t.Errorf("replace path = %q, want %q", put.Path, myJobPath)
	}
	var sent run.Job
	if err := json.Unmarshal(put.Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Metadata == nil || sent.Metadata.ResourceVersion != jobVersion {
		t.Errorf("sent resourceVersion = %+v, want %q", sent.Metadata, jobVersion)
	}
}

// TestJobDiffIgnoresServerManagedFields checks that what the server adds to a job — who changed it
// and when, which changes on every write — does not show up as a difference.
func TestJobDiffIgnoresServerManagedFields(t *testing.T) {
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) {
		return http.StatusOK, liveJob("gcr.io/project/job:v1")
	})
	diff, err := c.CompareManifest(context.Background(), KindJob, "my-job", []byte(validJobManifest),
		"job.yaml", PlanOptions{})
	if err != nil {
		t.Fatalf("CompareManifest() error = %v", err)
	}
	if diff != "" {
		t.Errorf("diff = %s, want none", diff)
	}
}

func TestJobConflictMentionsExecutions(t *testing.T) {
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if r.Method == http.MethodPut {
			return http.StatusConflict, googleAPIError(409, "version mismatch")
		}
		return http.StatusOK, liveJob("gcr.io/project/job:v1")
	})
	manifest := strings.Replace(validJobManifest, "job:v1", "job:v2", 1)
	plan, err := c.Plan(context.Background(), KindJob, "my-job", []byte(manifest), PlanOptions{})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	_, err = plan.Apply(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), `job "my-job" changed after the diff was computed`) ||
		!strings.Contains(err.Error(), "starting an execution") {
		t.Errorf("Apply() error = %v, want the conflict explained, executions included", err)
	}
}

func TestPlanJobResolvesServerDefaults(t *testing.T) {
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if r.Method == http.MethodPost {
			resolved := liveJob("gcr.io/project/job:v1")
			resolved.Spec.Template.Spec.TaskCount = 1
			return http.StatusOK, resolved
		}
		return http.StatusNotFound, googleAPIError(404, "not found")
	})
	plan, err := c.Plan(context.Background(), KindJob, "my-job", []byte(validJobManifest),
		PlanOptions{ResolveDefaults: true})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	post := lastRequest(t, api, http.MethodPost)
	if !strings.Contains(post.Query, "dryRun=all") {
		t.Errorf("defaults resolved with %s?%s, want a dry-run create", post.Path, post.Query)
	}
	if !strings.Contains(plan.Diff, "+      taskCount: 1") {
		t.Errorf("diff does not show the resolved default:\n%s", plan.Diff)
	}
}

func TestPlanJobRefusesToKeepTraffic(t *testing.T) {
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) {
		return http.StatusOK, liveJob("gcr.io/project/job:v1")
	})
	_, err := c.Plan(context.Background(), KindJob, "my-job", []byte(validJobManifest), PlanOptions{KeepTraffic: true})
	if err == nil || !strings.Contains(err.Error(), "no traffic") {
		t.Errorf("Plan(KeepTraffic) error = %v, want a refusal", err)
	}
}

func TestDeployPlanWaitsForTheJob(t *testing.T) {
	polls := 0
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		job := liveJob("gcr.io/project/job:v1")
		if r.Method == http.MethodGet {
			polls++
			if polls <= 2 { // the plan's GET, then one poll before the controller catches up
				job.Status.ObservedGeneration = 2
			}
		}
		return http.StatusOK, job
	})
	plan, err := c.Plan(context.Background(), KindJob, "my-job", []byte(validJobManifest), PlanOptions{})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	var updates []string
	status, err := plan.Wait(context.Background(), WaitOptions{
		Generation: 3,
		Interval:   1,
		OnUpdate:   func(m string) { updates = append(updates, m) },
	})
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if status.ObservedGeneration != 3 {
		t.Errorf("Wait() = %+v, want it to return once generation 3 is observed", status)
	}
	if len(updates) == 0 || !strings.Contains(updates[0], "generation 3 not observed yet") {
		t.Errorf("updates = %q, want the wait to have waited for the generation", updates)
	}
	if got := lastRequest(t, api, http.MethodGet).Path; got != myJobPath {
		t.Errorf("polled %q, want the job", got)
	}
}

func TestJobWaitReportsAFailedJob(t *testing.T) {
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) {
		job := liveJob("gcr.io/project/job:v1")
		job.Status.Conditions = []*run.GoogleCloudRunV1Condition{
			{Type: "Ready", Status: "False", Reason: "ContainerMissing", Message: "image not found"},
		}
		return http.StatusOK, job
	})
	plan, err := c.Plan(context.Background(), KindJob, "my-job", []byte(validJobManifest), PlanOptions{})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	_, err = plan.Wait(context.Background(), WaitOptions{Interval: 1})
	if err == nil || !strings.Contains(err.Error(), `job "my-job" failed to become ready`) {
		t.Errorf("Wait() error = %v, want the job's failure", err)
	}
}

// TestVerifyRemoteChecksAJob checks that the existence checks read a job's containers, one
// template deeper than a service's.
func TestVerifyRemoteChecksAJob(t *testing.T) {
	recorded, opts := startVerifyAPI(t, func(string) int { return http.StatusNotFound })
	manifest := strings.Replace(validJobManifest, "          containers:\n",
		"          serviceAccountName: runner@p.iam.gserviceaccount.com\n          containers:\n", 1)
	manifest = strings.Replace(manifest, "gcr.io/project/job:v1", "us-docker.pkg.dev/p/repo/job:v1", 1)

	res, err := VerifyRemote(context.Background(), KindJob, testProject, testRegion, []byte(manifest), opts...)
	if err != nil {
		t.Fatalf("VerifyRemote() error = %v", err)
	}
	if len(res.Missing) != 2 {
		t.Errorf("Missing = %v, want the job's service account and image", res.Missing)
	}
	if !containsPath(recorded(), "/v1/projects/-/serviceAccounts/runner@p.iam.gserviceaccount.com") {
		t.Errorf("requests = %v, want the service account looked up", recorded())
	}
}
