package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// pinnedServiceJSON は rollback 済みのサービス。トラフィックが古いリビジョンに固定
// されていて、最新の Ready なリビジョンには何も向いていない。
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

// startServiceAPI は指定したサービス定義とリビジョン一覧に応えるフェイク API を立て、
// 適用 (PUT) の body を拾えるようにする。
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

// trafficTargets は適用された body から spec.traffic を取り出す。
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

// TestTrafficSplitsAgainstTheServingRevision は、--percent が 100 未満のときに残りが
// いま配信しているリビジョンへ残ることを確認する (カナリアの形)。
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

// TestTrafficToLatestUnpinsTheSplit は、--to-latest がリビジョン名の固定を外すことを
// 確認する。rollback 後にここへ戻れないと、最新へ進む手段が deploy しか無くなる。
func TestTrafficToLatestUnpinsTheSplit(t *testing.T) {
	// rollback 済み (トラフィックが古いリビジョンに固定されている) 状態から始める。
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

// TestTrafficRejectsAnUnknownRevision は、このサービスに属さないリビジョン名を
// 適用前に弾くことを確認する。通すと、どこにも届かない配分ができる。
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

// TestTrafficValidatesTheFlagsFirst は、フラグの組み合わせが target の解決や認証より
// 先に検証されることを確認する (--project も渡していないので、順序が逆なら
// "project is required" が出る)。
func TestTrafficValidatesTheFlagsFirst(t *testing.T) {
	_, _, err := executeRoot(t, "traffic", "my-svc", "--to", "my-svc-00006-def", "--to-latest")
	if err == nil {
		t.Fatal("traffic error = nil, want the flag combination to be rejected")
	}
	if !strings.Contains(err.Error(), "cannot be combined") {
		t.Errorf("traffic error = %v, want the flag error rather than a target/credential error", err)
	}
}

// TestDeployNoTrafficPinsTheCurrentSplit は、--no-traffic が現在の配分をリビジョン名で
// 固定して送ることを確認する。latestRevision のままだと、これから作るリビジョンが
// 全量を受け取ってしまい「トラフィックを向けない」にならない。
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
