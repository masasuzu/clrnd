package cmd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// manifestWithSecret is a manifest for which exactly one remote existence check runs.
const manifestWithSecret = `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      - image: gcr.io/project/image:new
        env:
        - name: TOKEN
          valueFrom:
            secretKeyRef:
              name: api-token
              key: latest
`

// decodeVerifyJSON parses the output of verify --format json.
func decodeVerifyJSON(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("failed to parse the JSON output %q: %v", stdout, err)
	}
	return out
}

// TestVerifyJSONReportsSuccess checks that on success only a single object with ok:true is
// written to stdout.
func TestVerifyJSONReportsSuccess(t *testing.T) {
	manifest := writeManifest(t, localManifest)

	stdout, stderr, err := executeRoot(t, "verify", "my-svc", manifest, "--local-only", "--format", "json")
	if err != nil {
		t.Fatalf("verify error = %v", err)
	}
	got := decodeVerifyJSON(t, stdout)
	if got["ok"] != true {
		t.Errorf("ok = %v, want true", got["ok"])
	}
	if strings.Contains(stderr, "warning") {
		t.Errorf("stderr = %q, want warnings in the JSON instead", stderr)
	}
}

// TestVerifyJSONReportsMissingAndStillFails checks that Missing is reported in structured form
// while a failure is still returned for the exit code. Either one alone is unusable from CI.
func TestVerifyJSONReportsMissingAndStillFails(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error": {"code": 404, "message": "not found"}}`))
	})
	manifest := writeManifest(t, manifestWithSecret)

	stdout, _, err := executeRoot(t, "verify", "my-svc", manifest, "--format", "json",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil {
		t.Fatal("verify error = nil, want the missing secret to fail the command")
	}
	got := decodeVerifyJSON(t, stdout)
	if got["ok"] != false {
		t.Errorf("ok = %v, want false", got["ok"])
	}
	missing, _ := got["missing"].([]any)
	if len(missing) != 1 || !strings.Contains(missing[0].(string), "api-token") {
		t.Errorf("missing = %v, want the secret reported", got["missing"])
	}
}

// TestVerifyJSONReportsUncheckedWithoutFailing checks that what could not be checked is reported
// as a warning while the command itself succeeds (a mere lack of permission must not turn CI red).
func TestVerifyJSONReportsUncheckedWithoutFailing(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error": {"code": 403, "message": "denied"}}`))
	})
	manifest := writeManifest(t, manifestWithSecret)

	stdout, _, err := executeRoot(t, "verify", "my-svc", manifest, "--format", "json",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("verify error = %v, want a permission problem to stay a warning", err)
	}
	got := decodeVerifyJSON(t, stdout)
	if got["ok"] != true {
		t.Errorf("ok = %v, want true", got["ok"])
	}
	if unchecked, _ := got["unchecked"].([]any); len(unchecked) == 0 {
		t.Errorf("unchecked = %v, want the check that could not be decided", got["unchecked"])
	}
}

// TestVerifyJSONReportsLocalErrors checks that local validation failures are structured too.
func TestVerifyJSONReportsLocalErrors(t *testing.T) {
	manifest := writeManifest(t, strings.Replace(localManifest, "name: my-svc", "name: other-svc", 1))

	stdout, _, err := executeRoot(t, "verify", "my-svc", manifest, "--local-only", "--format", "json")
	if err == nil {
		t.Fatal("verify error = nil, want the name mismatch to fail")
	}
	got := decodeVerifyJSON(t, stdout)
	if got["ok"] != false {
		t.Errorf("ok = %v, want false", got["ok"])
	}
	errs, _ := got["errors"].([]any)
	if len(errs) == 0 || !strings.Contains(errs[0].(string), "does not match") {
		t.Errorf("errors = %v, want the local validation failure", got["errors"])
	}
}

// TestVerifyRejectsAnInvalidFormat checks that a bad --format is rejected before target
// resolution and authentication (the same order as the other commands).
func TestVerifyRejectsAnInvalidFormat(t *testing.T) {
	manifest := writeManifest(t, localManifest)

	_, _, err := executeRoot(t, "verify", "my-svc", manifest, "--format", "yaml")
	if err == nil || !strings.Contains(err.Error(), "invalid --format") {
		t.Errorf("verify error = %v, want the format to be rejected", err)
	}
}

// TestVerifyJSONReportsAnImageOverrideFailure checks that JSON is written even when --image
// fails. If any path ends with stdout still empty, the `clrnd verify --format json | jq ...` the
// README recommends fails on unreadable output.
func TestVerifyJSONReportsAnImageOverrideFailure(t *testing.T) {
	manifest := writeManifest(t, localManifest)

	stdout, _, err := executeRoot(t, "verify", "my-svc", manifest, "--local-only", "--format", "json",
		"--image", "sidecar=gcr.io/project/image:v2")
	if err == nil {
		t.Fatal("verify error = nil, want the unknown container to fail")
	}
	got := decodeVerifyJSON(t, stdout)
	if got["ok"] != false {
		t.Errorf("ok = %v, want false", got["ok"])
	}
	if errs, _ := got["errors"].([]any); len(errs) == 0 {
		t.Errorf("errors = %v, want the failure reported in the JSON", got["errors"])
	}
}
