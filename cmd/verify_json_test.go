package cmd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// manifestWithSecret はリモート実在チェックが 1 件だけ走るマニフェスト。
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

// decodeVerifyJSON は verify --format json の出力を読む。
func decodeVerifyJSON(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("failed to parse the JSON output %q: %v", stdout, err)
	}
	return out
}

// TestVerifyJSONReportsSuccess は、成功時に ok:true の 1 オブジェクトだけが stdout へ
// 出ることを確認する。
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

// TestVerifyJSONReportsMissingAndStillFails は、Missing を構造化して出しつつ、終了
// コードのための失敗も返すことを確認する。片方だけでは CI から使えない。
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

// TestVerifyJSONReportsUncheckedWithoutFailing は、確認できなかったものが警告として
// 出つつ、コマンド自体は成功することを確認する (権限が無いだけで CI を赤くしない)。
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

// TestVerifyJSONReportsLocalErrors は、ローカル検証の失敗も構造化されることを確認する。
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

// TestVerifyRejectsAnInvalidFormat は、--format の誤りが target の解決や認証より先に
// 弾かれることを確認する (他のコマンドと同じ順序)。
func TestVerifyRejectsAnInvalidFormat(t *testing.T) {
	manifest := writeManifest(t, localManifest)

	_, _, err := executeRoot(t, "verify", "my-svc", manifest, "--format", "yaml")
	if err == nil || !strings.Contains(err.Error(), "invalid --format") {
		t.Errorf("verify error = %v, want the format to be rejected", err)
	}
}

// TestVerifyJSONReportsAnImageOverrideFailure は、--image の失敗でも JSON が出ることを
// 確認する。stdout が空のまま終わる経路があると、README が勧める
// `clrnd verify --format json | jq ...` が読めない出力で落ちる。
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
