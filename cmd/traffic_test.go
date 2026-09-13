package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// pinnedServiceJSON is a service after a rollback. Traffic is pinned to an older revision, and
// nothing is routed to the latest Ready revision.
const pinnedServiceJSON = `{
  "apiVersion": "serving.knative.dev/v1",
  "kind": "Service",
  "metadata": {"name": "my-svc", "namespace": "test-project", "generation": 7},
  "spec": {
    "template": {"spec": {"containers": [{"image": "gcr.io/project/image:new"}]}},
    "traffic": [{"revisionName": "my-svc-00006-def", "percent": 100}]
  },
  "status": {
    "observedGeneration": 7,
    "latestReadyRevisionName": "my-svc-00007-abc",
    "conditions": [{"type": "Ready", "status": "True"}],
    "traffic": [{"revisionName": "my-svc-00006-def", "percent": 100}]
  }
}`

// startServiceAPI starts a fake API that answers with the given service definition and the
// revision list, and makes the body of the apply (PUT) available.
func startServiceAPI(t *testing.T, serviceBody string) func() []byte {
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
		_, _ = w.Write([]byte(serviceBody))
	})

	return func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return put
	}
}

// trafficTargets extracts spec.traffic from the applied body.
func trafficTargets(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("failed to parse the applied body: %v", err)
	}
	spec, _ := sent["spec"].(map[string]any)
	raw, _ := spec["traffic"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		target, _ := entry.(map[string]any)
		out = append(out, target)
	}
	return out
}

// TestTrafficSplitsAgainstTheServingRevision checks that when --percent is below 100, the
// remainder stays on the revision currently serving traffic (the canary shape).
func TestTrafficSplitsAgainstTheServingRevision(t *testing.T) {
	sentBody := startRollbackAPI(t)

	if _, _, err := executeRoot(t, "traffic", "my-svc", "--to", "my-svc-00006-def",
		"--percent", "10", "--auto-approve", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("traffic error = %v", err)
	}

	targets := trafficTargets(t, sentBody())
	if len(targets) != 2 {
		t.Fatalf("spec.traffic = %v, want two targets", targets)
	}
	if targets[0]["revisionName"] != "my-svc-00006-def" || targets[0]["percent"] != float64(10) {
		t.Errorf("spec.traffic[0] = %v, want 10%% to my-svc-00006-def", targets[0])
	}
	if targets[1]["revisionName"] != "my-svc-00007-abc" || targets[1]["percent"] != float64(90) {
		t.Errorf("spec.traffic[1] = %v, want the remaining 90%% on the serving revision", targets[1])
	}
}

// TestTrafficToLatestUnpinsTheSplit checks that --to-latest removes the pinning by revision name.
// Without a way back to this after a rollback, deploy would be the only way to move forward to
// the latest revision.
func TestTrafficToLatestUnpinsTheSplit(t *testing.T) {
	// Start from the state after a rollback (traffic pinned to an older revision).
	sentBody := startServiceAPI(t, pinnedServiceJSON)

	if _, _, err := executeRoot(t, "traffic", "my-svc", "--to-latest",
		"--auto-approve", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("traffic error = %v", err)
	}

	targets := trafficTargets(t, sentBody())
	if len(targets) != 1 {
		t.Fatalf("spec.traffic = %v, want a single target", targets)
	}
	if targets[0]["latestRevision"] != true || targets[0]["percent"] != float64(100) {
		t.Errorf("spec.traffic[0] = %v, want 100%% following the latest revision", targets[0])
	}
	if _, pinned := targets[0]["revisionName"]; pinned {
		t.Errorf("spec.traffic[0] = %v, want no revision name", targets[0])
	}
}

// TestTrafficRejectsAnUnknownRevision checks that a revision name that does not belong to this
// service is rejected before applying. Letting it through would create a split that reaches
// nothing.
func TestTrafficRejectsAnUnknownRevision(t *testing.T) {
	sentBody := startRollbackAPI(t)

	_, _, err := executeRoot(t, "traffic", "my-svc", "--to", "other-00001-xyz",
		"--auto-approve", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil {
		t.Fatal("traffic error = nil, want an unknown revision to be rejected")
	}
	if !strings.Contains(err.Error(), "does not belong to this service") {
		t.Errorf("traffic error = %v", err)
	}
	if sentBody() != nil {
		t.Errorf("a body was applied (%s), want nothing applied", sentBody())
	}
}

// TestTrafficValidatesTheFlagsFirst checks that the flag combination is validated before target
// resolution and authentication (--project is not passed either, so if the order were reversed
// "project is required" would come out instead).
func TestTrafficValidatesTheFlagsFirst(t *testing.T) {
	_, _, err := executeRoot(t, "traffic", "my-svc", "--to", "my-svc-00006-def", "--to-latest")
	if err == nil {
		t.Fatal("traffic error = nil, want the flag combination to be rejected")
	}
	if !strings.Contains(err.Error(), "cannot be combined") {
		t.Errorf("traffic error = %v, want the flag error rather than a target/credential error", err)
	}
}

// TestDeployNoTrafficPinsTheCurrentSplit checks that --no-traffic sends the current split pinned
// by revision name. Left as latestRevision, the revision about to be created would receive all of
// the traffic, which is not "send it no traffic".
func TestDeployNoTrafficPinsTheCurrentSplit(t *testing.T) {
	sentBody := startRollbackAPI(t)

	manifest := writeManifest(t, localManifest)
	if _, _, err := executeRoot(t, "deploy", "my-svc", manifest, "--no-traffic",
		"--no-server-defaults", "--auto-approve", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("deploy --no-traffic error = %v", err)
	}

	targets := trafficTargets(t, sentBody())
	if len(targets) != 1 {
		t.Fatalf("spec.traffic = %v, want the current split pinned", targets)
	}
	if targets[0]["revisionName"] != "my-svc-00007-abc" || targets[0]["percent"] != float64(100) {
		t.Errorf("spec.traffic[0] = %v, want 100%% pinned to the serving revision", targets[0])
	}
	if targets[0]["latestRevision"] == true {
		t.Error("spec.traffic[0].latestRevision = true, want the split pinned by name")
	}
}

// TestDeployImageOverrideIsWhatGetsApplied checks that the image overridden with --image is what
// actually gets applied. This is the typical CI case of "the manifest is fixed, only the tag is
// swapped".
func TestDeployImageOverrideIsWhatGetsApplied(t *testing.T) {
	sentBody := startRollbackAPI(t)

	manifest := writeManifest(t, localManifest)
	if _, _, err := executeRoot(t, "deploy", "my-svc", manifest,
		"--image", "gcr.io/project/image:from-ci",
		"--no-server-defaults", "--auto-approve", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("deploy --image error = %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(sentBody(), &sent); err != nil {
		t.Fatalf("failed to parse the applied body: %v", err)
	}
	spec, _ := sent["spec"].(map[string]any)
	template, _ := spec["template"].(map[string]any)
	templateSpec, _ := template["spec"].(map[string]any)
	containers, _ := templateSpec["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("containers = %v, want one", containers)
	}
	container, _ := containers[0].(map[string]any)
	if container["image"] != "gcr.io/project/image:from-ci" {
		t.Errorf("image = %v, want the overridden image", container["image"])
	}
}

// TestDiffImageOverrideMatchesDeploy checks that diff compares using the overridden image. If
// this did not match, the diff you saw and what gets applied would disagree.
func TestDiffImageOverrideMatchesDeploy(t *testing.T) {
	startServiceAPI(t, rollbackServiceJSON)

	manifest := writeManifest(t, localManifest)
	stdout, _, err := executeRoot(t, "diff", "my-svc", manifest,
		"--image", "gcr.io/project/image:from-ci", "--no-server-defaults",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("diff --image error = %v", err)
	}
	if !strings.Contains(stdout, "from-ci") {
		t.Errorf("diff stdout = %q, want the overridden image compared", stdout)
	}
}

// TestDeployImageOverrideRejectsAnUnknownContainer checks that a wrong override is rejected
// before anything is applied.
func TestDeployImageOverrideRejectsAnUnknownContainer(t *testing.T) {
	sentBody := startRollbackAPI(t)

	manifest := writeManifest(t, localManifest)
	_, _, err := executeRoot(t, "deploy", "my-svc", manifest,
		"--image", "sidecar=gcr.io/project/image:from-ci",
		"--auto-approve", "--no-wait",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil {
		t.Fatal("deploy error = nil, want the unknown container to be rejected")
	}
	if sentBody() != nil {
		t.Errorf("a body was applied (%s), want nothing applied", sentBody())
	}
}
