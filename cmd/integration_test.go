package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/masasuzu/clrnd/internal/config"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"google.golang.org/api/option"
)

// liveServiceJSON is the live service definition the fake API returns.
const liveServiceJSON = `{
  "apiVersion": "serving.knative.dev/v1",
  "kind": "Service",
  "metadata": {"name": "my-svc", "namespace": "test-project", "uid": "abc-123", "generation": 7},
  "spec": {"template": {"spec": {"containers": [{"image": "gcr.io/project/image:old"}]}}},
  "status": {"latestReadyRevisionName": "my-svc-00007-abc"}
}`

// liveServiceWithRevisionJSON is a live service definition. Cloud Run always fills in
// spec.template.metadata.name (the server-assigned revision name) when it is fetched.
const liveServiceWithRevisionJSON = `{
  "apiVersion": "serving.knative.dev/v1",
  "kind": "Service",
  "metadata": {"name": "my-svc", "namespace": "test-project", "uid": "abc-123", "generation": 7},
  "spec": {"template": {
    "metadata": {"name": "my-svc-00007-abc"},
    "spec": {"containers": [{"image": "gcr.io/project/image:old"}]}
  }},
  "status": {"latestReadyRevisionName": "my-svc-00007-abc"}
}`

// liveServiceStatusJSON is a live service definition that has every field status reads.
const liveServiceStatusJSON = `{
  "apiVersion": "serving.knative.dev/v1",
  "kind": "Service",
  "metadata": {"name": "my-svc", "namespace": "test-project", "generation": 7},
  "spec": {"template": {"spec": {"containers": [{"image": "gcr.io/project/image:old"}]}}},
  "status": {
    "url": "https://my-svc.a.run.app",
    "latestReadyRevisionName": "my-svc-00007-abc",
    "latestCreatedRevisionName": "my-svc-00007-abc",
    "observedGeneration": 7,
    "conditions": [
      {"type": "Ready", "status": "True"},
      {"type": "RoutesReady", "status": "True"}
    ],
    "traffic": [{"revisionName": "my-svc-00007-abc", "percent": 100, "latestRevision": true}]
  }
}`

// localManifest is the local manifest fed to cmd. It differs from live only in the image tag.
const localManifest = `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      - image: gcr.io/project/image:new
`

// echoDryRun answers a dry-run write by returning the body it was sent unchanged. It simulates "a
// server that adds no defaults", so going through --server-defaults leaves desired unchanged and
// the diff stays exactly the manifest. It returns true when it has responded.
func echoDryRun(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPut || !strings.Contains(r.URL.RawQuery, "dryRun=all") {
		return false
	}
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
	return true
}

// startFakeAPI starts an httptest server that stands in for the Cloud Run Admin API and points
// clientOptions at it. It restores the original when the test ends.
func startFakeAPI(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	saved := clientOptions
	clientOptions = []option.ClientOption{
		option.WithEndpoint(srv.URL + "/"),
		option.WithHTTPClient(srv.Client()),
	}
	t.Cleanup(func() { clientOptions = saved })
}

// resetFlags puts flag values that cobra does not roll back to their defaults. The flags are
// bound to package variables, so without this a value from one test leaks into the next.
func resetFlags(c *cobra.Command) {
	c.Flags().VisitAll(func(f *pflag.Flag) {
		// For StringArray and the like, Set appends, so slice flags are emptied with Replace.
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			_ = sv.Replace(nil)
		} else {
			_ = f.Value.Set(f.DefValue)
		}
		f.Changed = false
	})
	for _, sub := range c.Commands() {
		resetFlags(sub)
	}
}

// clearTargetEnv empties the gcloud-compatible environment variables, so that test results do
// not change when CLOUDSDK_CORE_PROJECT or the like is set in a developer's or CI environment.
func clearTargetEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{envProjectPrimary, envProjectSecondary, envRegionPrimary, envRegionSecondary} {
		t.Setenv(key, "")
	}
}

// executeRoot runs the root command with the given arguments and returns stdout/stderr.
// rootCmd is a package variable, so cfg, the flags and the environment variables are reset on
// every run to keep state from leaking between tests.
func executeRoot(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	clearTargetEnv(t)

	savedCfg, savedDir := cfg, configDir
	t.Cleanup(func() { cfg, configDir = savedCfg, savedDir })
	cfg, configDir = &config.Config{}, ""

	resetFlags(rootCmd)
	t.Cleanup(func() { resetFlags(rootCmd) })

	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	// Make stdin explicitly non-interactive. Without this it falls back to os.Stdin,
	// isInteractive becomes true only when go test is started from a terminal, and the
	// confirmation-prompt tests give different results locally and in CI.
	rootCmd.SetIn(strings.NewReader(""))
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetIn(nil)
		// Resetting to nil would make cobra read os.Args[1:] (= the go test flags).
		rootCmd.SetArgs([]string{})
	})

	err = rootCmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

// writeManifest writes a manifest into a temporary directory and returns its path.
func writeManifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write the manifest: %v", err)
	}
	return path
}

// TestDiffEndToEnd checks the whole path end to end: flag resolution → client creation → API
// fetch → normalization → diff output.
func TestDiffEndToEnd(t *testing.T) {
	var gotPath string
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if echoDryRun(w, r) {
			return
		}
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceJSON))
	})

	manifest := writeManifest(t, localManifest)
	stdout, _, err := executeRoot(t, "diff", "my-svc", manifest,
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("diff error = %v", err)
	}

	wantPath := "/apis/serving.knative.dev/v1/namespaces/test-project/services/my-svc"
	if gotPath != wantPath {
		t.Errorf("requested path = %q, want %q", gotPath, wantPath)
	}
	if !strings.Contains(stdout, "-      - image: gcr.io/project/image:old") ||
		!strings.Contains(stdout, "+      - image: gcr.io/project/image:new") {
		t.Errorf("diff stdout = %q, want the image change", stdout)
	}
	// Server-managed fields are dropped by normalization, so they do not appear in the diff.
	if strings.Contains(stdout, "uid:") || strings.Contains(stdout, "status:") {
		t.Errorf("diff stdout leaks server-managed fields:\n%s", stdout)
	}
}

// TestDiffUsesConfigFile checks that project/region/service/manifest can all be resolved from the
// config file alone.
func TestDiffUsesConfigFile(t *testing.T) {
	var gotPath string
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if echoDryRun(w, r) {
			return
		}
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceJSON))
	})

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(localManifest), 0o600); err != nil {
		t.Fatalf("failed to write the manifest: %v", err)
	}
	configFile := filepath.Join(dir, "clrnd.yml")
	configYAML := "project: test-project\nregion: asia-northeast1\nservice: my-svc\nmanifest: manifest.yaml\n"
	if err := os.WriteFile(configFile, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("failed to write the config: %v", err)
	}

	// Pass neither positional arguments nor flags: check that service / manifest / project /
	// region are all resolved from the config.
	stdout, _, err := executeRoot(t, "diff", "--config", configFile)
	if err != nil {
		t.Fatalf("diff error = %v", err)
	}
	// The config's project shows up in the request path (not one taken from the environment).
	wantPath := "/apis/serving.knative.dev/v1/namespaces/test-project/services/my-svc"
	if gotPath != wantPath {
		t.Errorf("requested path = %q, want %q", gotPath, wantPath)
	}
	if !strings.Contains(stdout, "+      - image: gcr.io/project/image:new") {
		t.Errorf("diff stdout = %q, want the image change", stdout)
	}
}

// TestDeployDryRunEndToEnd checks that deploy prints the diff to stdout and calls ReplaceService
// with dryRun=all.
func TestDeployDryRunEndToEnd(t *testing.T) {
	var putQuery string
	var putBody []byte
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			putQuery = r.URL.RawQuery
			body, _ := io.ReadAll(r.Body)
			putBody = body
			// Act as a server that adds no defaults and return the body it was sent unchanged.
			// The apply under --dry-run is a dry run too, so it goes through here as well.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceJSON))
	})

	manifest := writeManifest(t, localManifest)
	stdout, _, err := executeRoot(t, "deploy", "my-svc", manifest,
		"--project", "test-project", "--region", "asia-northeast1", "--dry-run")
	if err != nil {
		t.Fatalf("deploy error = %v", err)
	}
	if !strings.Contains(stdout, "+      - image: gcr.io/project/image:new") {
		t.Errorf("deploy stdout = %q, want the diff", stdout)
	}
	if !strings.Contains(putQuery, "dryRun=all") {
		t.Errorf("ReplaceService query = %q, want dryRun=all", putQuery)
	}
	var sent map[string]interface{}
	if err := json.Unmarshal(putBody, &sent); err != nil {
		t.Fatalf("failed to parse the sent body: %v", err)
	}
	meta, _ := sent["metadata"].(map[string]interface{})
	if meta["namespace"] != "test-project" {
		t.Errorf("sent namespace = %v, want test-project", meta["namespace"])
	}
}

// TestDeployValidatesManifestBeforeResolvingTarget checks that local validation of the manifest
// happens before project/region resolution and client creation (ADC discovery).
// In the reverse order, in an environment with no credentials or target, a manifest problem
// hides behind a different error.
func TestDeployValidatesManifestBeforeResolvingTarget(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected API call to %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})

	// A manifest whose metadata.name disagrees with the service argument. project/region are
	// not set anywhere (executeRoot empties the environment variables, and there is no config).
	manifest := writeManifest(t, localManifest)
	_, _, err := executeRoot(t, "deploy", "other-svc", manifest)
	if err == nil {
		t.Fatal("deploy error = nil, want a manifest validation error")
	}
	if !strings.Contains(err.Error(), "does not match service argument") {
		t.Errorf("deploy error = %v, want the manifest problem to surface first", err)
	}
}

// TestVerifyLocalOnlyNeedsNoAPI checks that --local-only requires neither credentials nor the API
// (offline validation in CI).
func TestVerifyLocalOnlyNeedsNoAPI(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected API call to %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})

	manifest := writeManifest(t, localManifest)
	if _, _, err := executeRoot(t, "verify", "my-svc", manifest, "--local-only"); err != nil {
		t.Fatalf("verify --local-only error = %v", err)
	}
}

// TestInitScaffoldsWithoutRevisionName checks that init drops the live revision name when it
// writes out the manifest. If it were kept, the second deploy that changes the template would fail
// with "a revision with the same name cannot be recreated".
func TestInitScaffoldsWithoutRevisionName(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceWithRevisionJSON))
	})

	dir := t.TempDir()
	t.Chdir(dir)

	if _, _, err := executeRoot(t, "init", "my-svc",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("init error = %v", err)
	}

	manifest, err := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	if err != nil {
		t.Fatalf("failed to read the scaffolded manifest: %v", err)
	}
	if strings.Contains(string(manifest), "my-svc-00007-abc") {
		t.Errorf("scaffolded manifest pins the revision name:\n%s", manifest)
	}
	// The spec.template.metadata left empty must be gone along with it.
	if strings.Contains(string(manifest), "metadata: {}") {
		t.Errorf("scaffolded manifest keeps an empty template metadata:\n%s", manifest)
	}
}

// TestDiffIsEmptyRightAfterInit checks that the diff right after init is empty.
// Live always has a revision name and init drops it, so unless the live revision name is ignored
// in the comparison, a "diff that never goes away" keeps showing up.
func TestDiffIsEmptyRightAfterInit(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceWithRevisionJSON))
	})

	dir := t.TempDir()
	t.Chdir(dir)

	if _, _, err := executeRoot(t, "init", "my-svc",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("init error = %v", err)
	}

	// Have service/manifest/project/region resolved from the clrnd.yml init wrote.
	stdout, _, err := executeRoot(t, "diff")
	if err != nil {
		t.Fatalf("diff error = %v", err)
	}
	if stdout != "" {
		t.Errorf("diff right after init = %q, want empty", stdout)
	}
}

// TestVerifyWarnsWhenRevisionNameIsPinned checks that verify succeeds with a warning for a
// manifest that pins the revision name (it is the correct way to write a one-shot deploy, so it
// is not made a failure).
func TestVerifyWarnsWhenRevisionNameIsPinned(t *testing.T) {
	manifest := writeManifest(t, `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    metadata:
      name: my-svc-00007-abc
    spec:
      containers:
      - image: gcr.io/project/image:new
`)

	_, stderr, err := executeRoot(t, "verify", "my-svc", manifest, "--local-only")
	if err != nil {
		t.Fatalf("verify error = %v, want success with a warning", err)
	}
	if !strings.Contains(stderr, "warning:") || !strings.Contains(stderr, "my-svc-00007-abc") {
		t.Errorf("verify stderr = %q, want a warning naming the pinned revision", stderr)
	}
}

// TestDeployWarnsWhenRevisionNameIsPinned checks that deploy gives the same warning.
// A CI job that only runs deploy never gets to see verify's warning, and would see nothing but the
// 409 from Cloud Run with no clue to the cause.
func TestDeployWarnsWhenRevisionNameIsPinned(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceWithRevisionJSON))
	})

	manifest := writeManifest(t, `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    metadata:
      name: my-svc-00007-abc
    spec:
      containers:
      - image: gcr.io/project/image:new
`)

	_, stderr, err := executeRoot(t, "deploy", "my-svc", manifest,
		"--project", "test-project", "--region", "asia-northeast1", "--dry-run")
	if err != nil {
		t.Fatalf("deploy error = %v", err)
	}
	if !strings.Contains(stderr, "warning:") || !strings.Contains(stderr, "my-svc-00007-abc") {
		t.Errorf("deploy stderr = %q, want a warning naming the pinned revision", stderr)
	}
}

// TestDeployDoesNotWarnWithoutRevisionName checks that an ordinary manifest produces no warning.
func TestDeployDoesNotWarnWithoutRevisionName(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceJSON))
	})

	manifest := writeManifest(t, localManifest)
	_, stderr, err := executeRoot(t, "deploy", "my-svc", manifest,
		"--project", "test-project", "--region", "asia-northeast1", "--dry-run")
	if err != nil {
		t.Fatalf("deploy error = %v", err)
	}
	if strings.Contains(stderr, "warning:") {
		t.Errorf("deploy stderr = %q, want no warning", stderr)
	}
}

// TestVerifyDoesNotWarnWithoutRevisionName checks that an ordinary manifest with no revision name
// produces no warning.
func TestVerifyDoesNotWarnWithoutRevisionName(t *testing.T) {
	manifest := writeManifest(t, localManifest)

	_, stderr, err := executeRoot(t, "verify", "my-svc", manifest, "--local-only")
	if err != nil {
		t.Fatalf("verify error = %v", err)
	}
	if strings.Contains(stderr, "warning:") {
		t.Errorf("verify stderr = %q, want no warning", stderr)
	}
}

// TestStatusTextEndToEnd checks that status, by default (text), prints a readable summary.
func TestStatusTextEndToEnd(t *testing.T) {
	var gotPath string
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceStatusJSON))
	})

	stdout, stderr, err := executeRoot(t, "status", "my-svc",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("status error = %v", err)
	}

	wantPath := "/apis/serving.knative.dev/v1/namespaces/test-project/services/my-svc"
	if gotPath != wantPath {
		t.Errorf("requested path = %q, want %q", gotPath, wantPath)
	}
	for _, want := range []string{
		"Service:         my-svc",
		"URL:             https://my-svc.a.run.app",
		"Ready:           True",
		"Generation:      7 (observed 7)",
		"  100%  my-svc-00007-abc",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status stdout should contain %q:\n%s", want, stdout)
		}
	}
	// It is a read-only command, so nothing goes to stderr.
	if stderr != "" {
		t.Errorf("status stderr = %q, want empty", stderr)
	}
}

// TestStatusJSONEndToEnd checks that --format json produces machine-readable output.
func TestStatusJSONEndToEnd(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceStatusJSON))
	})

	stdout, _, err := executeRoot(t, "status", "my-svc", "--format", "json",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("status --format json error = %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("status --format json produced invalid JSON: %v\n%s", err, stdout)
	}
	if got["service"] != "my-svc" {
		t.Errorf("service = %v, want my-svc", got["service"])
	}
	if got["url"] != "https://my-svc.a.run.app" {
		t.Errorf("url = %v", got["url"])
	}
	conds, _ := got["conditions"].([]interface{})
	if len(conds) != 2 {
		t.Fatalf("conditions = %v, want 2 entries", got["conditions"])
	}
	first, _ := conds[0].(map[string]interface{})
	if first["type"] != "Ready" || first["status"] != "True" {
		t.Errorf("conditions[0] = %v", conds[0])
	}
}

// TestStatusRejectsInvalidFormat checks that an invalid --format is rejected before the client is
// created. In the reverse order, the flag mistake hides behind an authentication error.
func TestStatusRejectsInvalidFormat(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected API call to %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})

	_, _, err := executeRoot(t, "status", "my-svc", "--format", "yaml")
	if err == nil {
		t.Fatal("status error = nil, want an invalid --format error")
	}
	if !strings.Contains(err.Error(), `invalid --format "yaml"`) {
		t.Errorf("status error = %v, want it to name the bad format", err)
	}
}

// serviceJSON returns a service definition with the given generation / observedGeneration /
// Ready condition. When ready is empty, it has no Ready condition at all.
func serviceJSON(generation, observed int64, ready, reason string) string {
	conditions := ""
	if ready != "" {
		conditions = fmt.Sprintf(`"conditions": [{"type": "Ready", "status": %q, "reason": %q}], `, ready, reason)
	}
	return fmt.Sprintf(`{
  "apiVersion": "serving.knative.dev/v1",
  "kind": "Service",
  "metadata": {"name": "my-svc", "namespace": "test-project", "generation": %d},
  "spec": {"template": {"spec": {"containers": [{"image": "gcr.io/project/image:old"}]}}},
  "status": {%s"observedGeneration": %d}
}`, generation, conditions, observed)
}

// rolloutAPI starts a fake API that simulates the deploy -> wait flow.
// It changes the GET response before and after the PUT (the apply), and counts the GETs.
func rolloutAPI(t *testing.T, afterApply string) func() int {
	t.Helper()
	var mu sync.Mutex
	applied := false
	gets := 0

	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		// Resolving the defaults is a dry run, so it counts as neither an apply nor a poll.
		if echoDryRun(w, r) {
			return
		}
		mu.Lock()
		var body string
		switch r.Method {
		case http.MethodPut:
			applied = true
			// The apply response returns the new generation. deploy waits only for this
			// generation's rollout.
			body = serviceJSON(8, 7, "Unknown", "Deploying")
		default:
			gets++
			if applied {
				body = afterApply
			} else {
				body = serviceJSON(7, 7, "True", "")
			}
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})

	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return gets
	}
}

// TestWaitEndToEnd checks that wait waits until Ready and prints progress to stderr.
func TestWaitEndToEnd(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(serviceJSON(7, 7, "True", "")))
	})

	stdout, stderr, err := executeRoot(t, "wait", "my-svc", "--interval", "1ms",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("wait error = %v", err)
	}
	// On success nothing goes to stdout (stdout is for data only).
	if stdout != "" {
		t.Errorf("wait stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "waiting for my-svc") || !strings.Contains(stderr, "Ready=True") {
		t.Errorf("wait stderr = %q, want progress on stderr", stderr)
	}
}

// TestWaitFailsWhenTheRolloutFails checks that Ready=False fails without waiting further.
func TestWaitFailsWhenTheRolloutFails(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(serviceJSON(7, 7, "False", "RevisionFailed")))
	})

	_, _, err := executeRoot(t, "wait", "my-svc", "--interval", "1ms", "--timeout", "5s",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil {
		t.Fatal("wait error = nil, want a rollout failure")
	}
	if !strings.Contains(err.Error(), "RevisionFailed") {
		t.Errorf("wait error = %v, want the reason", err)
	}
}

// TestDeployFailsWhenTheRolloutFails is the heart of this PR. deploy used to exit 0 as soon as
// ReplaceService was accepted, so even when a revision failed to start, CI treated it as a
// success.
func TestDeployFailsWhenTheRolloutFails(t *testing.T) {
	gets := rolloutAPI(t, serviceJSON(8, 8, "False", "ConflictingRevisionName"))

	manifest := writeManifest(t, localManifest)
	_, stderr, err := executeRoot(t, "deploy", "my-svc", manifest, "--auto-approve",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil {
		t.Fatal("deploy error = nil, want the failed rollout to surface")
	}
	if !strings.Contains(err.Error(), "ConflictingRevisionName") {
		t.Errorf("deploy error = %v, want the rollout failure reason", err)
	}
	if !strings.Contains(stderr, "waiting for my-svc") {
		t.Errorf("deploy stderr = %q, want the wait progress", stderr)
	}
	if gets() < 2 {
		t.Errorf("GET count = %d, want the plan lookup plus at least one poll", gets())
	}
}

// TestDeployWaitsForTheRollout checks that deploy waits for a successful rollout and exits
// normally.
func TestDeployWaitsForTheRollout(t *testing.T) {
	gets := rolloutAPI(t, serviceJSON(8, 8, "True", ""))

	manifest := writeManifest(t, localManifest)
	_, stderr, err := executeRoot(t, "deploy", "my-svc", manifest, "--auto-approve",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("deploy error = %v", err)
	}
	if !strings.Contains(stderr, "Ready=True") {
		t.Errorf("deploy stderr = %q, want the rollout to be reported ready", stderr)
	}
	if gets() < 2 {
		t.Errorf("GET count = %d, want the plan lookup plus at least one poll", gets())
	}
}

// TestDeployRefusesWithoutConfirmation checks that in a non-interactive environment without
// --auto-approve, deploy fails without applying anything.
func TestDeployRefusesWithoutConfirmation(t *testing.T) {
	gets := rolloutAPI(t, serviceJSON(8, 8, "True", ""))

	manifest := writeManifest(t, localManifest)
	stdout, _, err := executeRoot(t, "deploy", "my-svc", manifest,
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil {
		t.Fatal("deploy error = nil, want a refusal without a terminal")
	}
	if !strings.Contains(err.Error(), "refusing to apply without confirmation") {
		t.Errorf("deploy error = %v", err)
	}
	// It refuses only after showing the diff.
	if !strings.Contains(stdout, "image:") {
		t.Errorf("deploy stdout = %q, want the diff to be shown before refusing", stdout)
	}
	// Only the Plan GET; nothing was applied or waited for.
	if gets() != 1 {
		t.Errorf("GET count = %d, want 1", gets())
	}
}

// TestDeployNoWaitSkipsTheWait checks that --no-wait returns as soon as the apply is accepted.
func TestDeployNoWaitSkipsTheWait(t *testing.T) {
	// Set up a state that would fail if it waited; succeeding anyway shows that it did not wait.
	gets := rolloutAPI(t, serviceJSON(8, 8, "False", "RevisionFailed"))

	manifest := writeManifest(t, localManifest)
	_, _, err := executeRoot(t, "deploy", "my-svc", manifest, "--auto-approve", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("deploy --no-wait error = %v", err)
	}
	// The only GET is the one for Plan. It did not poll.
	if gets() != 1 {
		t.Errorf("GET count = %d, want 1 (no polling with --no-wait)", gets())
	}
}

// revisionsJSON is the API response for the revision list.
const revisionsJSON = `{
  "apiVersion": "serving.knative.dev/v1",
  "kind": "RevisionList",
  "items": [
    {
      "metadata": {"name": "my-svc-00006-def", "creationTimestamp": "2026-08-21T09:00:00Z"},
      "spec": {"containers": [{"image": "gcr.io/project/image:old"}]},
      "status": {"conditions": [{"type": "Ready", "status": "True"}]}
    },
    {
      "metadata": {"name": "my-svc-00007-abc", "creationTimestamp": "2026-08-22T10:00:00Z"},
      "spec": {"containers": [{"image": "gcr.io/project/image:new"}]},
      "status": {"conditions": [{"type": "Ready", "status": "True"}]}
    }
  ]
}`

// startRevisionsAPI starts a fake API that answers both the service fetch and the revision list.
func startRevisionsAPI(t *testing.T) {
	t.Helper()
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/revisions") {
			_, _ = w.Write([]byte(revisionsJSON))
			return
		}
		_, _ = w.Write([]byte(liveServiceStatusJSON))
	})
}

// TestRevisionsTextEndToEnd checks that revisions prints a table sorted newest first.
func TestRevisionsTextEndToEnd(t *testing.T) {
	startRevisionsAPI(t)

	stdout, stderr, err := executeRoot(t, "revisions", "my-svc",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("revisions error = %v", err)
	}
	if stderr != "" {
		t.Errorf("revisions stderr = %q, want empty", stderr)
	}

	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("revisions stdout = %q, want a header and two rows", stdout)
	}
	if !strings.HasPrefix(lines[0], "REVISION") {
		t.Errorf("header = %q", lines[0])
	}
	// Newest first, joined with the live traffic.
	if !strings.HasPrefix(lines[1], "my-svc-00007-abc") || !strings.Contains(lines[1], "100%") {
		t.Errorf("first row = %q, want the newest revision with its traffic", lines[1])
	}
	if !strings.HasPrefix(lines[2], "my-svc-00006-def") || !strings.Contains(lines[2], "0%") {
		t.Errorf("second row = %q", lines[2])
	}
}

// TestRevisionsJSONEndToEnd checks that --format json prints an array.
func TestRevisionsJSONEndToEnd(t *testing.T) {
	startRevisionsAPI(t)

	stdout, _, err := executeRoot(t, "revisions", "my-svc", "--format", "json",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("revisions --format json error = %v", err)
	}

	var got []map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("revisions --format json produced invalid JSON: %v\n%s", err, stdout)
	}
	if len(got) != 2 {
		t.Fatalf("revisions --format json = %v, want 2 entries", got)
	}
	if got[0]["name"] != "my-svc-00007-abc" {
		t.Errorf("first entry = %v, want the newest revision", got[0])
	}
	if got[0]["percent"] != float64(100) {
		t.Errorf("percent = %v, want 100", got[0]["percent"])
	}
}

// TestRevisionsRejectsInvalidFormat checks that an invalid --format is rejected before the client
// is created.
func TestRevisionsRejectsInvalidFormat(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected API call to %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})

	_, _, err := executeRoot(t, "revisions", "my-svc", "--format", "yaml")
	if err == nil || !strings.Contains(err.Error(), `invalid --format "yaml"`) {
		t.Fatalf("revisions error = %v, want an invalid --format error", err)
	}
}

// rollbackServiceJSON is a live service with 100% of traffic routed to latestRevision.
const rollbackServiceJSON = `{
  "apiVersion": "serving.knative.dev/v1",
  "kind": "Service",
  "metadata": {"name": "my-svc", "namespace": "test-project", "generation": 7},
  "spec": {
    "template": {"spec": {"containers": [{"image": "gcr.io/project/image:new"}]}},
    "traffic": [{"latestRevision": true, "percent": 100}]
  },
  "status": {
    "observedGeneration": 7,
    "conditions": [{"type": "Ready", "status": "True"}],
    "traffic": [{"revisionName": "my-svc-00007-abc", "percent": 100, "latestRevision": true}]
  }
}`

// startRollbackAPI starts a fake API that answers the service, the revision list and the apply,
// and makes the PUT body available.
func startRollbackAPI(t *testing.T) func() []byte {
	t.Helper()
	var mu sync.Mutex
	var put []byte

	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			put = body
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/revisions") {
			_, _ = w.Write([]byte(revisionsJSON))
			return
		}
		_, _ = w.Write([]byte(rollbackServiceJSON))
	})

	return func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return put
	}
}

// TestRollbackEndToEnd checks that by default it reroutes 100% of traffic to the previous
// revision.
func TestRollbackEndToEnd(t *testing.T) {
	sentBody := startRollbackAPI(t)

	stdout, _, err := executeRoot(t, "rollback", "my-svc", "--auto-approve", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("rollback error = %v", err)
	}
	// The diff goes to stdout.
	if !strings.Contains(stdout, "my-svc-00006-def") {
		t.Errorf("rollback stdout = %q, want the diff to name the target revision", stdout)
	}

	var sent map[string]any
	if err := json.Unmarshal(sentBody(), &sent); err != nil {
		t.Fatalf("failed to parse the applied body: %v", err)
	}
	spec, _ := sent["spec"].(map[string]any)
	traffic, _ := spec["traffic"].([]any)
	if len(traffic) != 1 {
		t.Fatalf("spec.traffic = %v, want a single pinned target", spec["traffic"])
	}
	target, _ := traffic[0].(map[string]any)
	if target["revisionName"] != "my-svc-00006-def" || target["percent"] != float64(100) {
		t.Errorf("spec.traffic[0] = %v, want 100%% to my-svc-00006-def", target)
	}
	if target["latestRevision"] == true {
		t.Error("spec.traffic[0].latestRevision = true, want the traffic pinned")
	}
}

// TestRollbackToAnExplicitRevision checks that the revision given with --revision is used.
func TestRollbackToAnExplicitRevision(t *testing.T) {
	sentBody := startRollbackAPI(t)

	if _, _, err := executeRoot(t, "rollback", "my-svc", "--revision", "my-svc-00007-abc",
		"--auto-approve", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("rollback error = %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(sentBody(), &sent); err != nil {
		t.Fatalf("failed to parse the applied body: %v", err)
	}
	spec, _ := sent["spec"].(map[string]any)
	traffic, _ := spec["traffic"].([]any)
	target, _ := traffic[0].(map[string]any)
	if target["revisionName"] != "my-svc-00007-abc" {
		t.Errorf("spec.traffic[0] = %v, want the requested revision", target)
	}
}

// TestRollbackRejectsAnUnknownRevision checks that a revision that does not belong to this
// service is rejected before applying.
func TestRollbackRejectsAnUnknownRevision(t *testing.T) {
	sentBody := startRollbackAPI(t)

	_, _, err := executeRoot(t, "rollback", "my-svc", "--revision", "other-00001-xyz",
		"--auto-approve", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil {
		t.Fatal("rollback error = nil, want an unknown revision error")
	}
	if !strings.Contains(err.Error(), "does not belong to this service") {
		t.Errorf("rollback error = %v", err)
	}
	if len(sentBody()) != 0 {
		t.Error("rollback applied something despite the bad revision")
	}
}

// startDeleteAPI starts a fake API that answers the service fetch and the delete.
// When vanish is true, a GET after the DELETE returns 404 (the deletion has actually taken
// effect). When false, the service stays, which tells whether the command waits or not.
// It returns the number of DELETEs and the number of GETs after the DELETE.
func startDeleteAPI(t *testing.T, vanish bool) (func() int, func() int) {
	t.Helper()
	var mu sync.Mutex
	deletes, getsAfter := 0, 0

	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Method == http.MethodDelete {
			deletes++
		} else if deletes > 0 {
			getsAfter++
		}
		gone := vanish && deletes > 0 && r.Method != http.MethodDelete
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete:
			_, _ = w.Write([]byte(`{}`))
		case gone:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error": {"code": 404, "message": "not found"}}`))
		default:
			_, _ = w.Write([]byte(liveServiceStatusJSON))
		}
	})

	count := func(p *int) func() int {
		return func() int {
			mu.Lock()
			defer mu.Unlock()
			return *p
		}
	}
	return count(&deletes), count(&getsAfter)
}

// TestDeleteEndToEnd checks the whole sequence: showing what will be deleted on stderr, deleting
// it, and confirming it is gone. Deletion in Cloud Run is asynchronous, so returning without
// confirming makes a procedure like "delete, then recreate" race.
func TestDeleteEndToEnd(t *testing.T) {
	deletes, getsAfter := startDeleteAPI(t, true)

	stdout, stderr, err := executeRoot(t, "delete", "my-svc", "--auto-approve",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("delete error = %v", err)
	}
	if deletes() != 1 {
		t.Errorf("DELETE count = %d, want 1", deletes())
	}
	if getsAfter() < 1 {
		t.Errorf("GET count after the delete = %d, want at least 1 (it must confirm the service is gone)", getsAfter())
	}
	// Always show the project and region, so the wrong thing is not deleted by mistake.
	for _, want := range []string{"About to delete:", "service: my-svc", "project: test-project", "region:  asia-northeast1"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("delete stderr should contain %q:\n%s", want, stderr)
		}
	}
	// stdout is for data only. A delete has no data to print.
	if stdout != "" {
		t.Errorf("delete stdout = %q, want empty", stdout)
	}
}

// TestDeleteNoWaitSkipsTheWait checks that --no-wait returns as soon as the request is accepted.
func TestDeleteNoWaitSkipsTheWait(t *testing.T) {
	// The service never disappears if it waits, so succeeding shows that it did not wait.
	deletes, getsAfter := startDeleteAPI(t, false)

	if _, _, err := executeRoot(t, "delete", "my-svc", "--auto-approve", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("delete --no-wait error = %v", err)
	}
	if deletes() != 1 {
		t.Errorf("DELETE count = %d, want 1", deletes())
	}
	if getsAfter() != 0 {
		t.Errorf("GET count after the delete = %d, want 0 (no polling with --no-wait)", getsAfter())
	}
}

// TestDeleteRefusesWithoutConfirmation checks that in a non-interactive environment without
// --auto-approve, delete fails without deleting anything.
func TestDeleteRefusesWithoutConfirmation(t *testing.T) {
	deletes, _ := startDeleteAPI(t, true)

	_, _, err := executeRoot(t, "delete", "my-svc",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil {
		t.Fatal("delete error = nil, want a refusal without a terminal")
	}
	if !strings.Contains(err.Error(), "refusing to delete without confirmation") {
		t.Errorf("delete error = %v", err)
	}
	if deletes() != 0 {
		t.Errorf("DELETE count = %d, want 0 (nothing may be deleted without confirmation)", deletes())
	}
}

// TestDeleteDryRunDoesNotPrompt checks that --dry-run only validates, without confirming or
// waiting (so it passes in a non-interactive environment too).
func TestDeleteDryRunDoesNotPrompt(t *testing.T) {
	deletes, getsAfter := startDeleteAPI(t, false)

	if _, _, err := executeRoot(t, "delete", "my-svc", "--dry-run",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("delete --dry-run error = %v", err)
	}
	if deletes() != 1 {
		t.Errorf("DELETE count = %d, want 1 (a dry-run request is still sent)", deletes())
	}
	if getsAfter() != 0 {
		t.Errorf("GET count after the delete = %d, want 0 (nothing was deleted, so nothing to wait for)", getsAfter())
	}
}

// TestDeleteFailsWhenTheServiceIsMissing checks that a service that does not exist results in an
// error without asking for confirmation.
func TestDeleteFailsWhenTheServiceIsMissing(t *testing.T) {
	// The counter is written from the handler's goroutine, so it must always be guarded.
	// Unguarded, a regression that sends a DELETE either becomes a race the moment it happens,
	// or the test reads a stale 0 and misses the regression.
	var mu sync.Mutex
	deletes := 0
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			deletes++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error": {"code": 404, "message": "not found"}}`))
	})

	_, _, err := executeRoot(t, "delete", "missing", "--auto-approve",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil {
		t.Fatal("delete error = nil, want a not-found error")
	}
	mu.Lock()
	got := deletes
	mu.Unlock()
	if got != 0 {
		t.Errorf("DELETE count = %d, want 0", got)
	}
}

// TestRefreshEndToEnd checks that the definition is applied unchanged except for a new revision
// name. Cloud Run does not create a new revision unless spec.template changes, so this is the
// core of refresh.
func TestRefreshEndToEnd(t *testing.T) {
	sentBody := startRollbackAPI(t)

	stdout, _, err := executeRoot(t, "refresh", "my-svc", "--revision-suffix", "r260822190506",
		"--auto-approve", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("refresh error = %v", err)
	}
	if !strings.Contains(stdout, "my-svc-r260822190506") {
		t.Errorf("refresh stdout = %q, want the diff to show the new revision name", stdout)
	}

	var sent map[string]any
	if err := json.Unmarshal(sentBody(), &sent); err != nil {
		t.Fatalf("failed to parse the applied body: %v", err)
	}
	spec, _ := sent["spec"].(map[string]any)
	template, _ := spec["template"].(map[string]any)
	meta, _ := template["metadata"].(map[string]any)
	if meta["name"] != "my-svc-r260822190506" {
		t.Errorf("spec.template.metadata.name = %v, want the refreshed revision name", meta["name"])
	}
	// The definition itself is not changed.
	tspec, _ := template["spec"].(map[string]any)
	containers, _ := tspec["containers"].([]any)
	first, _ := containers[0].(map[string]any)
	if first["image"] != "gcr.io/project/image:new" {
		t.Errorf("image = %v, want the live definition to be reapplied unchanged", first["image"])
	}
}

// TestRefreshGeneratesARevisionName checks that a name is generated when --revision-suffix is
// omitted.
func TestRefreshGeneratesARevisionName(t *testing.T) {
	sentBody := startRollbackAPI(t)

	if _, _, err := executeRoot(t, "refresh", "my-svc", "--auto-approve", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("refresh error = %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(sentBody(), &sent); err != nil {
		t.Fatalf("failed to parse the applied body: %v", err)
	}
	spec, _ := sent["spec"].(map[string]any)
	template, _ := spec["template"].(map[string]any)
	meta, _ := template["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if !strings.HasPrefix(name, "my-svc-r") || len(name) != len("my-svc-r260822190506") {
		t.Errorf("spec.template.metadata.name = %q, want my-svc-r<UTC timestamp>", name)
	}
}

// TestRefreshRejectsAnInvalidRevisionName checks that a name Cloud Run would reject is rejected
// before applying.
func TestRefreshRejectsAnInvalidRevisionName(t *testing.T) {
	sentBody := startRollbackAPI(t)

	_, _, err := executeRoot(t, "refresh", "my-svc", "--revision-suffix", "Bad_Suffix",
		"--auto-approve", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil {
		t.Fatal("refresh error = nil, want an invalid revision name error")
	}
	if !strings.Contains(err.Error(), "not valid") {
		t.Errorf("refresh error = %v", err)
	}
	if len(sentBody()) != 0 {
		t.Error("refresh applied something despite the invalid name")
	}
}

// defaultedServiceJSON is the definition after Cloud Run has filled in the defaults.
// It contains fields that localManifest (the minimal one) does not have.
const defaultedServiceJSON = `{
  "apiVersion": "serving.knative.dev/v1",
  "kind": "Service",
  "metadata": {"name": "my-svc", "namespace": "test-project", "generation": 7},
  "spec": {
    "template": {"spec": {
      "containerConcurrency": 80,
      "timeoutSeconds": 300,
      "containers": [{"image": "gcr.io/project/image:new"}]
    }},
    "traffic": [{"latestRevision": true, "percent": 100}]
  },
  "status": {"observedGeneration": 7, "conditions": [{"type": "Ready", "status": "True"}]}
}`

// TestDiffServerDefaults checks that --server-defaults removes the part of the diff that comes
// from server defaults (issue #11).
func TestDiffServerDefaults(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(defaultedServiceJSON))
	})

	manifest := writeManifest(t, localManifest)
	target := []string{"--project", "test-project", "--region", "asia-northeast1"}

	// By default the server is asked to resolve the defaults, so even a minimal manifest has an
	// empty diff.
	resolved, _, err := executeRoot(t, append([]string{"diff", "my-svc", manifest}, target...)...)
	if err != nil {
		t.Fatalf("diff error = %v", err)
	}
	if resolved != "" {
		t.Errorf("diff = %q, want empty (server defaults are resolved by default)", resolved)
	}

	// With --no-server-defaults the manifest is compared as written, so the defaults show up in
	// the diff.
	plain, _, err := executeRoot(t,
		append([]string{"diff", "my-svc", manifest, "--no-server-defaults"}, target...)...)
	if err != nil {
		t.Fatalf("diff --no-server-defaults error = %v", err)
	}
	if !strings.Contains(plain, "containerConcurrency") {
		t.Errorf("diff --no-server-defaults = %q, want the defaults to show", plain)
	}
}

// unchangedServiceJSON is a live service with the same contents as localManifest.
// After normalization it matches desired, so deploy's diff is empty.
// ready / reason replace the Ready condition.
func unchangedServiceJSON(ready, reason string) string {
	return fmt.Sprintf(`{
  "apiVersion": "serving.knative.dev/v1",
  "kind": "Service",
  "metadata": {"name": "my-svc", "namespace": "test-project", "generation": 7},
  "spec": {"template": {"spec": {"containers": [{"image": "gcr.io/project/image:new"}]}}},
  "status": {"observedGeneration": 7, "conditions": [{"type": "Ready", "status": %q, "reason": %q}]}
}`, ready, reason)
}

// startUnchangedAPI starts a fake API that returns "a live service with the same contents as the
// manifest", and counts the GETs.
func startUnchangedAPI(t *testing.T, ready, reason string) func() int {
	t.Helper()
	var mu sync.Mutex
	gets := 0

	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if echoDryRun(w, r) {
			return
		}
		if r.Method == http.MethodGet {
			mu.Lock()
			gets++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(unchangedServiceJSON(ready, reason)))
	})

	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return gets
	}
}

// TestDeployWithNoChangesStillChecksTheRollout verifies that the service's health is checked even
// when the diff is empty. Re-running with the same manifest after a failed deploy produces an
// empty diff, so letting that case straight through would report success over a broken service.
func TestDeployWithNoChangesStillChecksTheRollout(t *testing.T) {
	gets := startUnchangedAPI(t, "False", "RevisionFailed")

	manifest := writeManifest(t, localManifest)
	_, stderr, err := executeRoot(t, "deploy", "my-svc", manifest, "--auto-approve",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil {
		t.Fatal("deploy error = nil, want the unhealthy service to surface")
	}
	if !strings.Contains(err.Error(), "RevisionFailed") {
		t.Errorf("deploy error = %v, want the rollout failure reason", err)
	}
	if !strings.Contains(stderr, "No changes.") {
		t.Errorf("deploy stderr = %q, want it to report that there is nothing to apply", stderr)
	}
	// In addition to the Plan GET, polling to check health has run.
	if gets() < 2 {
		t.Errorf("GET count = %d, want the plan lookup plus at least one health poll", gets())
	}
}

// TestDeployWithNoChangesSucceedsWhenHealthy checks that an empty diff succeeds when the service
// is healthy (no spurious failure is introduced).
func TestDeployWithNoChangesSucceedsWhenHealthy(t *testing.T) {
	startUnchangedAPI(t, "True", "")

	manifest := writeManifest(t, localManifest)
	stdout, stderr, err := executeRoot(t, "deploy", "my-svc", manifest, "--auto-approve",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("deploy error = %v", err)
	}
	if !strings.Contains(stderr, "No changes.") {
		t.Errorf("deploy stderr = %q, want it to report that there is nothing to apply", stderr)
	}
	if stdout != "" {
		t.Errorf("deploy stdout = %q, want empty (there is no diff to print)", stdout)
	}
}

// TestDeployWithNoChangesSkipsTheCheckWithNoWait checks that --no-wait also skips the health
// check.
func TestDeployWithNoChangesSkipsTheCheckWithNoWait(t *testing.T) {
	// Set up a state that would fail if it waited; succeeding anyway shows that it did not look.
	gets := startUnchangedAPI(t, "False", "RevisionFailed")

	manifest := writeManifest(t, localManifest)
	if _, _, err := executeRoot(t, "deploy", "my-svc", manifest, "--auto-approve", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("deploy --no-wait error = %v", err)
	}
	if gets() != 1 {
		t.Errorf("GET count = %d, want 1 (no health poll with --no-wait)", gets())
	}
}

// TestExitCode checks how exit codes are assigned. It matches terraform plan's
// -detailed-exitcode: 0 = no differences / 1 = error / 2 = differences.
func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "success", err: nil, want: 0},
		{name: "error", err: errors.New("boom"), want: 1},
		{name: "differences", err: errDiffFound, want: ExitCodeDiff},
		{name: "wrapped differences", err: fmt.Errorf("diff: %w", errDiffFound), want: ExitCodeDiff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExitCode(tt.err); got != tt.want {
				t.Errorf("ExitCode(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

// TestDiffExitCodeReportsDifferences checks that with --exit-code, differences end with 2.
// Without it, a CI job that thinks it is using diff for drift detection passes silently.
func TestDiffExitCodeReportsDifferences(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if echoDryRun(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceJSON))
	})

	manifest := writeManifest(t, localManifest)
	stdout, _, err := executeRoot(t, "diff", "my-svc", manifest, "--exit-code",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil {
		t.Fatal("diff --exit-code error = nil, want the difference to be reported")
	}
	if got := ExitCode(err); got != ExitCodeDiff {
		t.Errorf("ExitCode() = %d, want %d", got, ExitCodeDiff)
	}
	// The diff itself still goes to stdout as before.
	if !strings.Contains(stdout, "image:") {
		t.Errorf("diff stdout = %q, want the diff to still be printed", stdout)
	}
}

// TestDiffExitCodeIsQuietWithoutDifferences checks that it ends with 0 when there are no
// differences.
func TestDiffExitCodeIsQuietWithoutDifferences(t *testing.T) {
	startUnchangedAPI(t, "True", "")

	manifest := writeManifest(t, localManifest)
	stdout, _, err := executeRoot(t, "diff", "my-svc", manifest, "--exit-code",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("diff --exit-code error = %v, want success when there is no difference", err)
	}
	if stdout != "" {
		t.Errorf("diff stdout = %q, want empty", stdout)
	}
}

// TestDiffWithoutExitCodeSucceedsDespiteDifferences checks that the default behaviour is
// unchanged.
func TestDiffWithoutExitCodeSucceedsDespiteDifferences(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if echoDryRun(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceJSON))
	})

	manifest := writeManifest(t, localManifest)
	stdout, _, err := executeRoot(t, "diff", "my-svc", manifest,
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("diff error = %v, want success without --exit-code", err)
	}
	if !strings.Contains(stdout, "image:") {
		t.Errorf("diff stdout = %q, want the diff", stdout)
	}
}

// TestRuntimeErrorsDoNotPrintUsage checks that a runtime error does not print the full usage.
// If it did, the carefully built error message would be buried under the flag list.
func TestRuntimeErrorsDoNotPrintUsage(t *testing.T) {
	manifest := writeManifest(t, localManifest)
	// Neither project nor region can be resolved, so it fails before the client is created.
	stdout, stderr, err := executeRoot(t, "diff", "my-svc", manifest)
	if err == nil {
		t.Fatal("diff error = nil, want the missing target to fail")
	}
	// cobra prints usage with Println (= OutOrStderr), so in a test that replaces SetOut it
	// lands on stdout. In the real binary it goes to stderr. It must not appear on either,
	// so look at both streams.
	combined := stdout + stderr
	if strings.Contains(combined, "Usage:") || strings.Contains(combined, "Flags:") {
		t.Errorf("the usage block should not be printed for a runtime error:\nstdout=%q\nstderr=%q",
			stdout, stderr)
	}
}

// TestVersion checks that --version prints the version.
func TestVersion(t *testing.T) {
	stdout, _, err := executeRoot(t, "--version")
	if err != nil {
		t.Fatalf("--version error = %v", err)
	}
	if !strings.Contains(stdout, "clrnd version") {
		t.Errorf("--version stdout = %q, want it to name the binary and version", stdout)
	}
}
