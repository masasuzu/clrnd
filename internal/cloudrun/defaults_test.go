package cloudrun

import (
	"context"
	"net/http"
	"strings"
	"testing"

	run "google.golang.org/api/run/v1"
)

// defaultedService は Cloud Run が既定値を埋めたあとのサービス定義。
// validManifest には無いフィールドが入っている。
func defaultedService() *run.Service {
	return &run.Service{
		ApiVersion: manifestAPIVersion,
		Kind:       manifestKind,
		Metadata:   &run.ObjectMeta{Name: "my-svc", Namespace: testProject},
		Spec: &run.ServiceSpec{
			Template: &run.RevisionTemplate{
				Spec: &run.RevisionSpec{
					ContainerConcurrency: 80,
					TimeoutSeconds:       300,
					Containers:           []*run.Container{{Image: "gcr.io/project/image:tag"}},
				},
			},
			Traffic: []*run.TrafficTarget{{LatestRevision: true, Percent: 100}},
		},
	}
}

// defaultingAPI は既定値を埋めるサーバを模す。dryRun=all の PUT には既定値入りの
// 定義を返し、GET でも同じものを返す。dryRun でない PUT の body は記録する。
func defaultingAPI(t *testing.T) (*Client, func() []recordedRequest) {
	t.Helper()
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		return http.StatusOK, defaultedService()
	})
	return c, api.recorded
}

func TestPlanWithoutResolveDefaultsShowsThem(t *testing.T) {
	c, _ := defaultingAPI(t)

	plan, err := c.Plan(context.Background(), "my-svc", []byte(validManifest), PlanOptions{})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	// 最小マニフェストなので、サーバが埋めた分がそのまま差分になる (issue #11)。
	for _, want := range []string{"containerConcurrency", "timeoutSeconds", "latestRevision"} {
		if !strings.Contains(plan.Diff, want) {
			t.Errorf("Plan().Diff should contain %q without --server-defaults:\n%s", want, plan.Diff)
		}
	}
}

func TestPlanResolvesServerDefaults(t *testing.T) {
	c, recorded := defaultingAPI(t)

	plan, err := c.Plan(context.Background(), "my-svc", []byte(validManifest),
		PlanOptions{ResolveDefaults: true})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if plan.Diff != "" {
		t.Errorf("Plan().Diff = %q, want empty once the defaults are resolved", plan.Diff)
	}

	// 解決には dryRun=all の書き込み系リクエストを使う (何も変えない)。
	var dryRuns int
	for _, r := range recorded() {
		if r.Method == http.MethodPut && strings.Contains(r.Query, "dryRun=all") {
			dryRuns++
		}
	}
	if dryRuns != 1 {
		t.Errorf("dry-run PUT count = %d, want 1", dryRuns)
	}
}

// TestPlanAppliesTheOriginalManifest は、既定値を解決しても適用に送るのは元の
// マニフェストのままであることを確認する。サーバが埋めた値を書き戻すと、将来
// Cloud Run の既定値が変わったときに古い値へ固定してしまう。
func TestPlanAppliesTheOriginalManifest(t *testing.T) {
	c, recorded := defaultingAPI(t)

	plan, err := c.Plan(context.Background(), "my-svc", []byte(validManifest),
		PlanOptions{ResolveDefaults: true})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if _, err := plan.Apply(context.Background(), false); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	var applied string
	for _, r := range recorded() {
		if r.Method == http.MethodPut && !strings.Contains(r.Query, "dryRun") {
			applied = string(r.Body)
		}
	}
	if applied == "" {
		t.Fatal("no real PUT was recorded")
	}
	for _, unwanted := range []string{"containerConcurrency", "timeoutSeconds", "latestRevision"} {
		if strings.Contains(applied, unwanted) {
			t.Errorf("the applied body contains the server default %q:\n%s", unwanted, applied)
		}
	}
}

func TestCompareManifestResolvesServerDefaults(t *testing.T) {
	c, _ := defaultingAPI(t)

	withDefaults, err := c.CompareManifest(context.Background(), "my-svc", []byte(validManifest),
		"manifest.yaml", PlanOptions{ResolveDefaults: true})
	if err != nil {
		t.Fatalf("CompareManifest() error = %v", err)
	}
	if withDefaults != "" {
		t.Errorf("CompareManifest() = %q, want empty once the defaults are resolved", withDefaults)
	}

	plain, err := c.CompareManifest(context.Background(), "my-svc", []byte(validManifest),
		"manifest.yaml", PlanOptions{})
	if err != nil {
		t.Fatalf("CompareManifest() error = %v", err)
	}
	if !strings.Contains(plain, "containerConcurrency") {
		t.Errorf("CompareManifest() = %q, want the defaults to show without the option", plain)
	}
}

// TestCompareManifestValidatesBeforeTheDryRun は、--server-defaults のとき
// サービス名の不一致を API に投げる前に弾くことを確認する。dry-run は path の
// サービス名と body の metadata.name が一致していないと 400 になるので、投げると
// 「権限が要る」と誤読される紛らわしいエラーになる。
func TestCompareManifestValidatesBeforeTheDryRun(t *testing.T) {
	c, recorded := defaultingAPI(t)

	_, err := c.CompareManifest(context.Background(), "other-svc", []byte(validManifest),
		"manifest.yaml", PlanOptions{ResolveDefaults: true})
	if err == nil {
		t.Fatal("CompareManifest() error = nil, want the name mismatch to be caught locally")
	}
	if !strings.Contains(err.Error(), "does not match service argument") {
		t.Errorf("CompareManifest() error = %v, want the local validation error", err)
	}
	for _, r := range recorded() {
		if r.Method == http.MethodPut {
			t.Error("a dry-run request was sent despite the local validation failure")
		}
	}
}

// TestCompareManifestSetsTheNamespace は、dry-run に送る定義が deploy と同じ
// 前処理 (namespace を送信先に合わせる) を通ることを確認する。これが無いと、
// namespace 付きのマニフェストで diff --server-defaults だけが API に弾かれる。
func TestCompareManifestSetsTheNamespace(t *testing.T) {
	c, recorded := defaultingAPI(t)

	manifest := []byte(`apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
  namespace: "123456789"
spec:
  template:
    spec:
      containers:
      - image: gcr.io/project/image:tag
`)
	if _, err := c.CompareManifest(context.Background(), "my-svc", manifest,
		"manifest.yaml", PlanOptions{ResolveDefaults: true}); err != nil {
		t.Fatalf("CompareManifest() error = %v", err)
	}

	var sent string
	for _, r := range recorded() {
		if r.Method == http.MethodPut {
			sent = string(r.Body)
		}
	}
	if !strings.Contains(sent, `"namespace":"`+testProject+`"`) {
		t.Errorf("the dry-run body = %s, want the namespace replaced with the target project", sent)
	}
}

// TestResolveDefaultsErrorPointsAtTheFlag は、権限不足のときに何をすればよいかを
// 伝えることを確認する。dry-run は書き込み系なので、読むだけの権限では通らない。
func TestResolveDefaultsErrorPointsAtTheFlag(t *testing.T) {
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if r.Method == http.MethodPut {
			return http.StatusForbidden, googleAPIError(403, "permission denied")
		}
		return http.StatusOK, defaultedService()
	})

	_, err := c.Plan(context.Background(), "my-svc", []byte(validManifest),
		PlanOptions{ResolveDefaults: true})
	if err == nil {
		t.Fatal("Plan() error = nil, want the permission failure to surface")
	}
	for _, want := range []string{
		"failed to resolve server defaults",
		"permission denied",    // 原因をそのまま見せる
		"--no-server-defaults", // 対処を示す
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Plan() error = %v, want it to contain %q", err, want)
		}
	}
}
