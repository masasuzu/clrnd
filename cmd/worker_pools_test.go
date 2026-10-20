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

// liveWorkerPoolJSON is the live worker pool the fake API returns. It differs from
// localWorkerPoolManifest only in the image tag, and is ready at the generation it reports.
const liveWorkerPoolJSON = `{
  "apiVersion": "run.googleapis.com/v1",
  "kind": "WorkerPool",
  "metadata": {"name": "my-pool", "namespace": "test-project", "generation": 5, "resourceVersion": "rv-5",
    "annotations": {"run.googleapis.com/scalingMode": "manual", "run.googleapis.com/manualInstanceCount": "2"}},
  "spec": {"template": {"spec": {"containers": [{"image": "gcr.io/project/worker:old"}]}}},
  "status": {"observedGeneration": 5, "conditions": [{"type": "Ready", "status": "True"}],
    "latestReadyRevisionName": "my-pool-00005-abc", "latestCreatedRevisionName": "my-pool-00005-abc",
    "instanceSplits": [{"revisionName": "my-pool-00005-abc", "percent": 100}]}
}`

const localWorkerPoolManifest = `apiVersion: run.googleapis.com/v1
kind: WorkerPool
metadata:
  name: my-pool
spec:
  template:
    spec:
      containers:
      - image: gcr.io/project/worker:new
`

const workerPoolPath = "/apis/run.googleapis.com/v1/namespaces/test-project/workerpools/my-pool"

// workerPoolAPI answers like the worker pools API: the live worker pool on GET, and a write's body
// echoed back. It records the method and path of every request.
func workerPoolAPI(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(liveWorkerPoolJSON))
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

// noAPI fails the test on any request: for checks that must finish before anything reaches the
// API.
func noAPI(t *testing.T) {
	t.Helper()
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected API call to %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
}

// TestWorkerPoolsDeployEndToEnd checks the whole deploy flow for a worker pool: diff, replace
// against the version the diff was computed from, then a wait on the worker pool.
func TestWorkerPoolsDeployEndToEnd(t *testing.T) {
	calls := workerPoolAPI(t)
	manifest := writeManifest(t, localWorkerPoolManifest)

	stdout, stderr, err := executeRoot(t, "worker-pools", "deploy", "my-pool", manifest, "--auto-approve",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("worker-pools deploy error = %v", err)
	}
	if !strings.Contains(stdout, "+      - image: gcr.io/project/worker:new") {
		t.Errorf("worker-pools deploy stdout = %q, want the diff", stdout)
	}
	for _, want := range []string{"worker pool: my-pool", "waiting for my-pool: observed generation 5, Ready=True"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("worker-pools deploy stderr = %q, want %q", stderr, want)
		}
	}

	var replaced bool
	for _, c := range calls() {
		if strings.HasPrefix(c, "PUT "+workerPoolPath+"?") && !strings.Contains(c, "dryRun") {
			replaced = true
		}
		if strings.Contains(c, "/services") || strings.Contains(c, "/jobs") {
			t.Errorf("request %q went to another kind's API", c)
		}
	}
	if !replaced {
		t.Errorf("requests = %q, want a replace of the worker pool", calls())
	}
}

func TestWorkerPoolsStatusEndToEnd(t *testing.T) {
	workerPoolAPI(t)

	stdout, _, err := executeRoot(t, "worker-pools", "status", "my-pool",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("worker-pools status error = %v", err)
	}
	for _, want := range []string{"Worker pool:     my-pool", "Ready:           True", "Instances:       2 (manual)",
		"Instance split:\n  100%  my-pool-00005-abc"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("worker-pools status stdout does not contain %q:\n%s", want, stdout)
		}
	}

	stdout, _, err = executeRoot(t, "worker-pools", "status", "my-pool", "--format", "json",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("worker-pools status --format json error = %v", err)
	}
	var got struct {
		WorkerPool    string `json:"workerPool"`
		InstanceCount *int   `json:"instanceCount"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("worker-pools status --format json printed %q: %v", stdout, err)
	}
	if got.WorkerPool != "my-pool" || got.InstanceCount == nil || *got.InstanceCount != 2 {
		t.Errorf("worker-pools status JSON = %s", stdout)
	}
}

func TestWorkerPoolsDeleteEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	deleted := false
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete:
			deleted = true
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && !deleted:
			_, _ = w.Write([]byte(liveWorkerPoolJSON))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error": {"code": 404, "message": "not found"}}`))
		}
	})

	stdout, stderr, err := executeRoot(t, "worker-pools", "delete", "my-pool", "--auto-approve", "--interval", "1ms",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("worker-pools delete error = %v", err)
	}
	for _, want := range []string{"About to delete:", "worker pool: my-pool", "instances:   2 (stopped with it)"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("worker-pools delete stderr does not contain %q:\n%s", want, stderr)
		}
	}
	if stdout != "" {
		t.Errorf("worker-pools delete stdout = %q, want empty", stdout)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) < 3 || calls[1] != "DELETE "+workerPoolPath || calls[len(calls)-1] != "GET "+workerPoolPath {
		t.Errorf("requests = %q, want a lookup, the delete, then polls until it is gone", calls)
	}
}

// TestWorkerPoolsRefuseManualScalingWithoutACount checks that the manifest the API would turn into
// a stopped worker pool is refused by every command that reads one, before anything reaches the
// API.
func TestWorkerPoolsRefuseManualScalingWithoutACount(t *testing.T) {
	noAPI(t)
	manifest := writeManifest(t, strings.Replace(localWorkerPoolManifest, "  name: my-pool\n",
		"  name: my-pool\n  annotations:\n    run.googleapis.com/scalingMode: manual\n", 1))
	for _, sub := range []string{"verify", "diff", "deploy"} {
		_, _, err := executeRoot(t, "worker-pools", sub, "my-pool", manifest,
			"--project", "test-project", "--region", "asia-northeast1")
		if err == nil || !strings.Contains(err.Error(), "sets the worker pool to 0 instances") {
			t.Errorf("worker-pools %s error = %v, want the 0-instance refusal", sub, err)
		}
	}
}

func TestWorkerPoolsVerifyLocalOnly(t *testing.T) {
	noAPI(t)
	manifest := writeManifest(t, localWorkerPoolManifest)

	stdout, _, err := executeRoot(t, "worker-pools", "verify", "my-pool", manifest, "--local-only", "--format", "json")
	if err != nil {
		t.Fatalf("worker-pools verify error = %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("worker-pools verify --format json printed %q: %v", stdout, err)
	}
	if result["workerPool"] != "my-pool" || result["ok"] != true {
		t.Errorf("result = %v, want workerPool my-pool and ok", result)
	}
	if _, ok := result["service"]; ok {
		t.Errorf("result = %v, want no service key for a worker pool", result)
	}
}

// TestWorkerPoolManifestKindMismatch checks that a worker pool manifest given to the service or
// job commands points at 'clrnd worker-pools', and the reverse, before anything needs the API.
func TestWorkerPoolManifestKindMismatch(t *testing.T) {
	noAPI(t)
	poolManifest := writeManifest(t, localWorkerPoolManifest)
	serviceManifest := writeManifest(t, localManifest)
	jobManifest := writeManifest(t, localJobManifest)

	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"deploy", "my-pool", poolManifest}, want: "use the 'clrnd worker-pools' commands"},
		{args: []string{"jobs", "verify", "my-pool", poolManifest}, want: "use the 'clrnd worker-pools' commands"},
		{args: []string{"worker-pools", "deploy", "my-svc", serviceManifest}, want: "use the top-level commands"},
		{args: []string{"worker-pools", "diff", "my-job", jobManifest}, want: "use the 'clrnd jobs' commands"},
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

// TestConfigWorkerPoolKey checks that the worker pool commands read the worker pool from the
// config, and that the service commands refuse a config that names one.
func TestConfigWorkerPoolKey(t *testing.T) {
	calls := workerPoolAPI(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pool.yaml"), []byte(localWorkerPoolManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(dir, "clrnd.yml")
	configYAML := "project: test-project\nregion: asia-northeast1\nworkerPool: my-pool\nmanifest: pool.yaml\n"
	if err := os.WriteFile(configFile, []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := executeRoot(t, "worker-pools", "diff", "--config", configFile)
	if err != nil {
		t.Fatalf("worker-pools diff error = %v", err)
	}
	if !strings.Contains(stdout, "gcr.io/project/worker:new") {
		t.Errorf("worker-pools diff stdout = %q, want the diff of the configured worker pool", stdout)
	}

	before := len(calls())
	for _, args := range [][]string{{"diff"}, {"status"}, {"jobs", "delete"}} {
		_, _, err := executeRoot(t, append(args, "--config", configFile)...)
		if err == nil || !strings.Contains(err.Error(), "the config file names a worker pool (workerPool: my-pool)") {
			t.Errorf("%v error = %v, want it to refuse the worker pool's config", args, err)
		}
	}
	if got := calls()[before:]; len(got) != 0 {
		t.Errorf("requests = %q, want none from the other kinds' commands", got)
	}
}

// TestWorkerPoolsInitEndToEnd checks that init scaffolds a worker pool: the manifest without the
// pinned revision name, and a config naming it under workerPool.
func TestWorkerPoolsInitEndToEnd(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.Replace(liveWorkerPoolJSON, `"spec": {"template": {"spec"`,
			`"spec": {"template": {"metadata": {"name": "my-pool-pinned"}, "spec"`, 1)))
	})
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.yaml")
	configFile := filepath.Join(dir, "clrnd.yml")

	if _, _, err := executeRoot(t, "worker-pools", "init", "my-pool", "--output", manifest, "--config", configFile,
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("worker-pools init error = %v", err)
	}
	got, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "kind: WorkerPool") || strings.Contains(string(got), "my-pool-pinned") {
		t.Errorf("manifest = %s, want a worker pool without the pinned revision name", got)
	}
	config, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "workerPool: my-pool") || strings.Contains(string(config), "service:") {
		t.Errorf("config = %s, want workerPool: my-pool and no service", config)
	}
}

func TestWorkerPoolsDeployHasNoTrafficFlag(t *testing.T) {
	manifest := writeManifest(t, localWorkerPoolManifest)
	_, _, err := executeRoot(t, "worker-pools", "deploy", "my-pool", manifest, "--no-traffic")
	if err == nil || !strings.Contains(err.Error(), "unknown flag: --no-traffic") {
		t.Errorf("worker-pools deploy --no-traffic error = %v, want an unknown flag", err)
	}
}

// TestWorkerPoolsGroupRejectsAnUnknownCommand checks that a mistyped subcommand fails rather than
// printing the help and exiting 0.
func TestWorkerPoolsGroupRejectsAnUnknownCommand(t *testing.T) {
	_, _, err := executeRoot(t, "worker-pools", "deplyo")
	if err == nil {
		t.Error("worker-pools deplyo succeeded, want an unknown command error")
	}
}
