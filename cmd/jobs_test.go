package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// liveJobJSON is the live job the fake API returns. It differs from localJobManifest only in the
// image tag, and is ready at the generation it reports.
const liveJobJSON = `{
  "apiVersion": "run.googleapis.com/v1",
  "kind": "Job",
  "metadata": {"name": "my-job", "namespace": "test-project", "generation": 4, "resourceVersion": "rv-4"},
  "spec": {"template": {"spec": {"template": {"spec": {"containers": [{"image": "gcr.io/project/job:old"}]}}}}},
  "status": {"observedGeneration": 4, "conditions": [{"type": "Ready", "status": "True"}]}
}`

const localJobManifest = `apiVersion: run.googleapis.com/v1
kind: Job
metadata:
  name: my-job
spec:
  template:
    spec:
      template:
        spec:
          containers:
          - image: gcr.io/project/job:new
`

const jobPath = "/apis/run.googleapis.com/v1/namespaces/test-project/jobs/my-job"

// jobAPI answers like the jobs API: the live job on GET, and a write's body echoed back (as a
// server that adds no defaults). It records the method and path of every request.
func jobAPI(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(liveJobJSON))
			return
		}
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(body)
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func TestJobsDiffEndToEnd(t *testing.T) {
	calls := jobAPI(t)
	manifest := writeManifest(t, localJobManifest)

	stdout, _, err := executeRoot(t, "jobs", "diff", "my-job", manifest,
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("jobs diff error = %v", err)
	}
	if !strings.Contains(stdout, "+          - image: gcr.io/project/job:new") {
		t.Errorf("jobs diff stdout = %q, want the image change", stdout)
	}
	got := calls()
	if len(got) == 0 || got[0] != "GET "+jobPath+"?alt=json&prettyPrint=false" {
		t.Errorf("requests = %q, want the job fetched first", got)
	}
}

// TestJobsDeployEndToEnd checks the whole deploy flow for a job: diff, replace against the version
// the diff was computed from, then a wait on the job.
func TestJobsDeployEndToEnd(t *testing.T) {
	calls := jobAPI(t)
	manifest := writeManifest(t, localJobManifest)

	stdout, stderr, err := executeRoot(t, "jobs", "deploy", "my-job", manifest, "--auto-approve",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("jobs deploy error = %v", err)
	}
	if !strings.Contains(stdout, "+          - image: gcr.io/project/job:new") {
		t.Errorf("jobs deploy stdout = %q, want the diff", stdout)
	}
	if !strings.Contains(stderr, "waiting for my-job: observed generation 4, Ready=True") {
		t.Errorf("jobs deploy stderr = %q, want the wait reported", stderr)
	}

	var replaced bool
	for _, c := range calls() {
		if strings.HasPrefix(c, "PUT "+jobPath+"?") && !strings.Contains(c, "dryRun") {
			replaced = true
		}
		if strings.Contains(c, "/services") {
			t.Errorf("request %q went to the services API", c)
		}
	}
	if !replaced {
		t.Errorf("requests = %q, want a replace of the job", calls())
	}
}

func TestJobsDeployDryRunSendsTheResourceVersion(t *testing.T) {
	var putBody []byte
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			putBody, _ = io.ReadAll(r.Body)
			_, _ = w.Write(putBody)
			return
		}
		_, _ = w.Write([]byte(liveJobJSON))
	})
	manifest := writeManifest(t, localJobManifest)

	if _, _, err := executeRoot(t, "jobs", "deploy", "my-job", manifest, "--dry-run",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("jobs deploy --dry-run error = %v", err)
	}
	var sent struct {
		Metadata struct {
			Namespace       string `json:"namespace"`
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(putBody, &sent); err != nil {
		t.Fatalf("failed to parse the sent body %s: %v", putBody, err)
	}
	if sent.Metadata.Namespace != "test-project" || sent.Metadata.ResourceVersion != "rv-4" {
		t.Errorf("sent metadata = %+v, want the project's namespace and the live resourceVersion", sent.Metadata)
	}
}

func TestJobsVerifyLocalOnly(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected API call to %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	manifest := writeManifest(t, localJobManifest)

	stdout, _, err := executeRoot(t, "jobs", "verify", "my-job", manifest, "--local-only", "--format", "json")
	if err != nil {
		t.Fatalf("jobs verify error = %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("jobs verify --format json printed %q: %v", stdout, err)
	}
	if result["job"] != "my-job" || result["ok"] != true {
		t.Errorf("result = %v, want job my-job and ok", result)
	}
	if _, ok := result["service"]; ok {
		t.Errorf("result = %v, want no service key for a job", result)
	}
}

// TestManifestKindMismatch checks that each kind's commands refuse the other's manifest before
// anything needs the API, and say which commands it belongs to.
func TestManifestKindMismatch(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected API call to %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	jobManifest := writeManifest(t, localJobManifest)
	serviceManifest := writeManifest(t, localManifest)

	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"deploy", "my-job", jobManifest}, want: "use the 'clrnd jobs' commands"},
		{args: []string{"diff", "my-job", jobManifest}, want: "use the 'clrnd jobs' commands"},
		{args: []string{"verify", "my-job", jobManifest}, want: "use the 'clrnd jobs' commands"},
		{args: []string{"jobs", "deploy", "my-svc", serviceManifest}, want: "use the top-level commands"},
		{args: []string{"jobs", "diff", "my-svc", serviceManifest}, want: "use the top-level commands"},
		{args: []string{"jobs", "verify", "my-svc", serviceManifest}, want: "use the top-level commands"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args[:len(tt.args)-2], " "), func(t *testing.T) {
			_, _, err := executeRoot(t, append(tt.args, "--project", "test-project", "--region", "asia-northeast1")...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// TestConfigJobKey checks that the jobs commands read the job from the config, and that the
// service commands refuse a config that names a job rather than act on a service of that name.
func TestConfigJobKey(t *testing.T) {
	calls := jobAPI(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "job.yaml"), []byte(localJobManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(dir, "clrnd.yml")
	configYAML := "project: test-project\nregion: asia-northeast1\njob: my-job\nmanifest: job.yaml\n"
	if err := os.WriteFile(configFile, []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := executeRoot(t, "jobs", "diff", "--config", configFile)
	if err != nil {
		t.Fatalf("jobs diff error = %v", err)
	}
	if !strings.Contains(stdout, "gcr.io/project/job:new") {
		t.Errorf("jobs diff stdout = %q, want the diff of the configured job", stdout)
	}

	before := len(calls())
	for _, sub := range []string{"diff", "status", "delete"} {
		_, _, err := executeRoot(t, sub, "--config", configFile)
		if err == nil || !strings.Contains(err.Error(), "the config file names a job (job: my-job)") {
			t.Errorf("%s error = %v, want it to refuse the job's config", sub, err)
		}
	}
	if got := calls()[before:]; len(got) != 0 {
		t.Errorf("requests = %q, want none from the service commands", got)
	}
}

func TestJobsDeployHasNoTrafficFlag(t *testing.T) {
	manifest := writeManifest(t, localJobManifest)
	_, _, err := executeRoot(t, "jobs", "deploy", "my-job", manifest, "--no-traffic")
	if err == nil || !strings.Contains(err.Error(), "unknown flag: --no-traffic") {
		t.Errorf("jobs deploy --no-traffic error = %v, want an unknown flag", err)
	}
}
