package cloudrun

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"google.golang.org/api/googleapi"
	run "google.golang.org/api/run/v1"
)

const validManifest = `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      - image: gcr.io/project/image:tag
`

func TestValidate(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		service  string
		wantErr  string // 期待するエラー部分文字列。空なら成功 (nil) を期待。
	}{
		{
			name:     "valid",
			manifest: validManifest,
			service:  "my-svc",
			wantErr:  "",
		},
		{
			name:     "service name mismatch",
			manifest: validManifest,
			service:  "other",
			wantErr:  "does not match",
		},
		{
			name: "wrong apiVersion",
			manifest: `apiVersion: v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      - image: gcr.io/x/y
`,
			service: "my-svc",
			wantErr: "apiVersion must be",
		},
		{
			name: "wrong kind",
			manifest: `apiVersion: serving.knative.dev/v1
kind: Deployment
metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      - image: gcr.io/x/y
`,
			service: "my-svc",
			wantErr: "kind must be",
		},
		{
			name: "missing metadata.name",
			manifest: `apiVersion: serving.knative.dev/v1
kind: Service
spec:
  template:
    spec:
      containers:
      - image: gcr.io/x/y
`,
			service: "my-svc",
			wantErr: "metadata.name is required",
		},
		{
			name: "no containers",
			manifest: `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec: {}
`,
			service: "my-svc",
			wantErr: "at least one container",
		},
		{
			name: "missing image",
			manifest: `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      - {}
`,
			service: "my-svc",
			wantErr: "image is required",
		},
		{
			// #1 で修正した nil パニックの回帰テスト。
			name: "null container",
			manifest: `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      - null
`,
			service: "my-svc",
			wantErr: "must not be null",
		},
		{
			// UnmarshalStrict が未知フィールド (typo) を検出する。
			name: "unknown field",
			manifest: `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      containerConcurency: 80
      containers:
      - image: gcr.io/x/y
`,
			service: "my-svc",
			wantErr: "unknown field",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate([]byte(tt.manifest), tt.service)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestValidateReportsMultipleProblems(t *testing.T) {
	manifest := `metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      - {}
`
	err := Validate([]byte(manifest), "my-svc")
	if err == nil {
		t.Fatal("Validate() = nil, want aggregated errors")
	}
	for _, want := range []string{"apiVersion must be", "kind must be", "image is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregated error %q missing %q", err.Error(), want)
		}
	}
}

func TestToManifestStripsServerManagedFields(t *testing.T) {
	svc := &run.Service{
		ApiVersion: "serving.knative.dev/v1",
		Kind:       "Service",
		Metadata: &run.ObjectMeta{
			Name:            "my-svc",
			Namespace:       "123456789",
			Uid:             "abc-uid",
			ResourceVersion: "rv-1",
			Generation:      2,
			Annotations: map[string]string{
				"run.googleapis.com/operation-id": "op-1",
				"run.googleapis.com/ingress":      "all",
			},
		},
		Status: &run.ServiceStatus{ObservedGeneration: 2},
	}

	out, err := ToManifest(svc)
	if err != nil {
		t.Fatalf("ToManifest() error = %v", err)
	}
	got := string(out)

	for _, stripped := range []string{"status", "abc-uid", "rv-1", "operation-id", "namespace", "observedGeneration"} {
		if strings.Contains(got, stripped) {
			t.Errorf("ToManifest() output should not contain %q:\n%s", stripped, got)
		}
	}
	for _, kept := range []string{"name: my-svc", "run.googleapis.com/ingress: all"} {
		if !strings.Contains(got, kept) {
			t.Errorf("ToManifest() output should contain %q:\n%s", kept, got)
		}
	}
}

// compareManifest は「live サービスとローカルのマニフェストを比較する」純粋な処理を
// テストから呼ぶためのヘルパ。本体では Client.CompareManifest / PlanService が
// 同じ compareServices を通る。
func compareManifest(current *run.Service, manifest []byte, currentName, desiredName string) (string, error) {
	desired, err := parseManifest(manifest)
	if err != nil {
		return "", err
	}
	return compareServices(current, desired, currentName, desiredName)
}

// normalize はテスト用に「ローカルのマニフェストを live 側と同じ正規化にそろえる」処理。
// かつて Normalize として公開していたが、今は Compare がこの経路を内包している。
func normalize(t *testing.T, manifest []byte) []byte {
	t.Helper()
	svc, err := parseManifest(manifest)
	if err != nil {
		t.Fatalf("parseManifest() error = %v", err)
	}
	out, err := ToManifest(svc)
	if err != nil {
		t.Fatalf("ToManifest() error = %v", err)
	}
	return out
}

func TestNormalizationStripsServerManagedFields(t *testing.T) {
	manifest := `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
  uid: abc-uid
  namespace: "123"
  annotations:
    run.googleapis.com/operation-id: op-1
    run.googleapis.com/ingress: all
status:
  observedGeneration: 2
`
	got := string(normalize(t, []byte(manifest)))

	for _, stripped := range []string{"status", "abc-uid", "operation-id", "namespace", "observedGeneration"} {
		if strings.Contains(got, stripped) {
			t.Errorf("normalized output should not contain %q:\n%s", stripped, got)
		}
	}
	if !strings.Contains(got, "run.googleapis.com/ingress: all") {
		t.Errorf("normalization should keep non-managed annotations:\n%s", got)
	}
}

func TestNormalizationIsIdempotent(t *testing.T) {
	first := normalize(t, []byte(validManifest))
	second := normalize(t, first)
	if string(first) != string(second) {
		t.Errorf("normalization is not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestCompareRejectsUnknownField(t *testing.T) {
	// Compare は parseManifest(strict) を通すため、diff も deploy と同様に未知
	// フィールド (typo) を弾く。これで両コマンドの挙動が一致する。
	manifest := []byte(`apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      containerConcurency: 80
      containers:
      - image: gcr.io/x/y
`)
	_, err := compareManifest(nil, manifest, "live", "local")
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("compareManifest() error = %v, want unknown field", err)
	}
}

func TestCompareIgnoresServerManagedFieldsInTheManifest(t *testing.T) {
	// ローカルのマニフェストにサーバ管理フィールドが残っていても、live 側と同じ正規化を
	// 通すので差分にはならない。
	manifest := []byte(`apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
  uid: abc-uid
  generation: 3
  annotations:
    serving.knative.dev/creator: someone@example.com
spec:
  template:
    spec:
      containers:
      - image: gcr.io/project/image:tag
status:
  observedGeneration: 3
`)
	live := &run.Service{
		ApiVersion: manifestAPIVersion,
		Kind:       manifestKind,
		Metadata:   &run.ObjectMeta{Name: "my-svc", Uid: "other-uid", Generation: 9},
		Spec: &run.ServiceSpec{Template: &run.RevisionTemplate{
			Spec: &run.RevisionSpec{Containers: []*run.Container{{Image: "gcr.io/project/image:tag"}}},
		}},
		Status: &run.ServiceStatus{ObservedGeneration: 9},
	}

	got, err := compareManifest(live, manifest, "live", "local")
	if err != nil {
		t.Fatalf("compareManifest() error = %v", err)
	}
	if got != "" {
		t.Errorf("compareManifest() = %q, want empty", got)
	}
}

func TestCompareTreatsMissingServiceAsFullAddition(t *testing.T) {
	got, err := compareManifest(nil, []byte(validManifest), "live/my-svc", "my-svc")
	if err != nil {
		t.Fatalf("compareManifest() error = %v", err)
	}
	if !strings.Contains(got, "+kind: Service") {
		t.Errorf("compareManifest() = %q, want the whole manifest added", got)
	}
}

func TestRevisionName(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     string
	}{
		{name: "not pinned", manifest: validManifest, want: ""},
		{
			name: "pinned",
			manifest: `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    metadata:
      name: my-svc-00007-abc
    spec:
      containers:
      - image: gcr.io/project/image:tag
`,
			want: "my-svc-00007-abc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RevisionName([]byte(tt.manifest))
			if err != nil {
				t.Fatalf("RevisionName() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("RevisionName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWithoutRevisionName(t *testing.T) {
	svc := &run.Service{Spec: &run.ServiceSpec{Template: &run.RevisionTemplate{
		Metadata: &run.ObjectMeta{Name: "my-svc-00007-abc", Annotations: map[string]string{"a": "b"}},
	}}}

	got := WithoutRevisionName(svc)
	if name := revisionName(got); name != "" {
		t.Errorf("revisionName() = %q, want empty", name)
	}
	// 名前以外の metadata は残す。
	if got.Spec.Template.Metadata.Annotations["a"] != "b" {
		t.Error("WithoutRevisionName should keep other metadata")
	}
	// 引数は書き換えない。
	if name := revisionName(svc); name != "my-svc-00007-abc" {
		t.Errorf("WithoutRevisionName mutated its argument: revisionName() = %q", name)
	}

	// リビジョン名が無ければそのまま返す (無駄なコピーをしない)。
	plain := &run.Service{}
	if WithoutRevisionName(plain) != plain {
		t.Error("WithoutRevisionName should return the same value when there is nothing to strip")
	}
	// spec.template.metadata が無くても panic しない。
	WithoutRevisionName(nil)
}

// TestCompareDoesNotMutateCurrent は Compare が引数の live サービスを書き換えないことを
// 確認する。書き換えると、比較後に live のリビジョン名を読む処理が静かに壊れる。
func TestCompareDoesNotMutateCurrent(t *testing.T) {
	live := liveService("gcr.io/project/image:tag")
	live.Spec.Template.Metadata = &run.ObjectMeta{Name: "my-svc-00007-abc"}

	if _, err := compareManifest(live, []byte(validManifest), "live", "local"); err != nil {
		t.Fatalf("compareManifest() error = %v", err)
	}
	if got := revisionName(live); got != "my-svc-00007-abc" {
		t.Errorf("compareManifest() mutated current: revisionName() = %q, want it untouched", got)
	}
}

// TestCompareIgnoresLiveRevisionNameWhenManifestOmitsIt は、リビジョン名を書いていない
// マニフェストに対して、live 側のサーバ採番されたリビジョン名が差分にならないことを
// 確認する。これが無いと init 直後の diff に消えない差分が出続ける。
func TestCompareIgnoresLiveRevisionNameWhenManifestOmitsIt(t *testing.T) {
	live := liveService("gcr.io/project/image:tag")
	live.Spec.Template.Metadata = &run.ObjectMeta{Name: "my-svc-00007-abc"}

	got, err := compareManifest(live, []byte(validManifest), "live", "local")
	if err != nil {
		t.Fatalf("compareManifest() error = %v", err)
	}
	if got != "" {
		t.Errorf("compareManifest() = %q, want empty (the live revision name must be ignored)", got)
	}
}

// TestCompareShowsRevisionNameWhenManifestPinsIt は、マニフェストが明示している場合は
// リビジョン名の違いを差分として見せることを確認する。
func TestCompareShowsRevisionNameWhenManifestPinsIt(t *testing.T) {
	live := liveService("gcr.io/project/image:tag")
	live.Spec.Template.Metadata = &run.ObjectMeta{Name: "my-svc-00007-abc"}

	manifest := []byte(`apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    metadata:
      name: my-svc-00008-def
    spec:
      containers:
      - image: gcr.io/project/image:tag
`)
	got, err := compareManifest(live, manifest, "live", "local")
	if err != nil {
		t.Fatalf("compareManifest() error = %v", err)
	}
	if !strings.Contains(got, "-      name: my-svc-00007-abc") ||
		!strings.Contains(got, "+      name: my-svc-00008-def") {
		t.Errorf("compareManifest() = %q, want the pinned revision name change", got)
	}
}

func TestDiff(t *testing.T) {
	a := []byte("metadata:\n  name: svc\nimage: foo\n")
	b := []byte("metadata:\n  name: svc\nimage: bar\n")

	t.Run("identical returns empty", func(t *testing.T) {
		out, err := Diff(a, a, "live", "local")
		if err != nil {
			t.Fatalf("Diff() error = %v", err)
		}
		if out != "" {
			t.Errorf("Diff() of identical input = %q, want empty", out)
		}
	})

	t.Run("difference is shown with markers", func(t *testing.T) {
		out, err := Diff(a, b, "live", "local")
		if err != nil {
			t.Fatalf("Diff() error = %v", err)
		}
		if !strings.Contains(out, "-image: foo") {
			t.Errorf("Diff() missing removed line:\n%s", out)
		}
		if !strings.Contains(out, "+image: bar") {
			t.Errorf("Diff() missing added line:\n%s", out)
		}
		if !strings.Contains(out, "live") || !strings.Contains(out, "local") {
			t.Errorf("Diff() missing file labels:\n%s", out)
		}
	})
}

func TestServiceContainers(t *testing.T) {
	t.Run("nil-safe on empty service", func(t *testing.T) {
		if got := serviceContainers(&run.Service{}); got != nil {
			t.Errorf("serviceContainers(empty) = %v, want nil", got)
		}
	})

	t.Run("returns containers", func(t *testing.T) {
		svc := &run.Service{
			Spec: &run.ServiceSpec{
				Template: &run.RevisionTemplate{
					Spec: &run.RevisionSpec{
						Containers: []*run.Container{{Image: "x"}},
					},
				},
			},
		}
		got := serviceContainers(svc)
		if len(got) != 1 || got[0].Image != "x" {
			t.Errorf("serviceContainers() = %v, want one container with image x", got)
		}
	})
}

func TestIsNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"404 googleapi", &googleapi.Error{Code: 404}, true},
		{"wrapped 404", fmt.Errorf("check: %w", &googleapi.Error{Code: 404}), true},
		{"403 googleapi", &googleapi.Error{Code: 403}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNotFound(tt.err); got != tt.want {
				t.Errorf("isNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestDeleteMapKeys(t *testing.T) {
	t.Run("removes keys and drops empty parent field", func(t *testing.T) {
		parent := map[string]interface{}{
			"annotations": map[string]interface{}{"a": "1", "b": "2"},
		}
		deleteMapKeys(parent, "annotations", []string{"a", "b"})
		if _, ok := parent["annotations"]; ok {
			t.Errorf("empty annotations map should have been removed: %v", parent)
		}
	})

	t.Run("keeps non-empty parent field", func(t *testing.T) {
		parent := map[string]interface{}{
			"annotations": map[string]interface{}{"a": "1", "keep": "2"},
		}
		deleteMapKeys(parent, "annotations", []string{"a"})
		ann, ok := parent["annotations"].(map[string]interface{})
		if !ok {
			t.Fatalf("annotations should remain: %v", parent)
		}
		if _, ok := ann["keep"]; !ok {
			t.Errorf("non-target key should be kept: %v", ann)
		}
	})

	t.Run("missing field is a no-op", func(t *testing.T) {
		parent := map[string]interface{}{"other": 1}
		deleteMapKeys(parent, "annotations", []string{"a"})
		if len(parent) != 1 {
			t.Errorf("unexpected mutation: %v", parent)
		}
	})
}

// TestCompareIgnoresServerManagedMetadata は、Cloud Run が勝手に付ける metadata が
// 手書きの最小マニフェストとの差分に出ないことを確認する。実際のサービス (gcloud で
// 作成したもの) から採った項目をそのまま並べている。--server-defaults を切ると
// この経路しか無いので、取りこぼすと「何をしても消えない差分」になる。
func TestCompareIgnoresServerManagedMetadata(t *testing.T) {
	current := &run.Service{
		ApiVersion: "serving.knative.dev/v1",
		Kind:       "Service",
		Metadata: &run.ObjectMeta{
			Name:      "my-svc",
			Namespace: "123456789",
			Labels: map[string]string{
				"cloud.googleapis.com/location": "asia-northeast1",
			},
			Annotations: map[string]string{
				"run.googleapis.com/client-name":    "gcloud",
				"run.googleapis.com/client-version": "1.2.3",
			},
			// 他のコントローラが管理しているサービスに付きうる read-only フィールド。
			Finalizers:      []string{"controller.example/finalizer"},
			OwnerReferences: []*run.OwnerReference{{Kind: "Thing", Name: "owner"}},
			GenerateName:    "my-svc-",
			ClusterName:     "cluster-1",
		},
		Spec: &run.ServiceSpec{
			Template: &run.RevisionTemplate{
				Metadata: &run.ObjectMeta{
					Labels: map[string]string{"client.knative.dev/nonce": "n-1"},
					Annotations: map[string]string{
						"run.googleapis.com/client-name": "gcloud",
					},
				},
				Spec: &run.RevisionSpec{
					Containers: []*run.Container{{Image: "gcr.io/p/img"}},
				},
			},
		},
	}

	manifest := `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      - image: gcr.io/p/img
`

	diff, err := compareManifest(current, []byte(manifest), "live/my-svc", "manifest.yaml")
	if err != nil {
		t.Fatalf("compareManifest() error = %v", err)
	}
	if diff != "" {
		t.Errorf("compareManifest() = %q, want no diff for server-managed metadata", diff)
	}
}

// TestPlanServiceKeepsTheResourceVersionItWasGiven は、live を読んでから編集する経路
// (rollback / refresh / traffic) が、自分が読んだ版に対して compare-and-swap すること
// を確認する。PlanService の GET は 2 回目なので、そちらの版に差し替えてしまうと、
// 2 つの GET の間に入った他人の変更を黙って巻き戻す。
func TestPlanServiceKeepsTheResourceVersionItWasGiven(t *testing.T) {
	var mu sync.Mutex
	gets := 0
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if r.Method != http.MethodGet {
			return http.StatusOK, readyService()
		}
		mu.Lock()
		gets++
		version := fmt.Sprintf("v%d", gets)
		mu.Unlock()
		svc := readyService()
		svc.Metadata.ResourceVersion = version
		return http.StatusOK, svc
	})

	// 1 回目の GET (呼び出し側が live を読む) の版を desired に載せる。
	live, err := c.GetService(context.Background(), "my-svc")
	if err != nil {
		t.Fatalf("GetService() error = %v", err)
	}
	if live.Metadata.ResourceVersion != "v1" {
		t.Fatalf("resourceVersion = %q, want the first read", live.Metadata.ResourceVersion)
	}

	plan, err := c.PlanService(context.Background(), "my-svc", live, PlanOptions{})
	if err != nil {
		t.Fatalf("PlanService() error = %v", err)
	}
	if got := plan.desired.Metadata.ResourceVersion; got != "v1" {
		t.Errorf("resourceVersion = %q, want the version the caller read (%q)", got, "v1")
	}
}

// TestPlanServiceStampsTheResourceVersionWhenThereIsNone は、マニフェスト由来の
// desired (版を持たない) には GET した版を載せることを確認する。載せないと Cloud Run は
// 無条件の上書きとして受け付け、並走した deploy が互いの変更を消す。
func TestPlanServiceStampsTheResourceVersionWhenThereIsNone(t *testing.T) {
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) {
		svc := readyService()
		svc.Metadata.ResourceVersion = "from-server"
		return http.StatusOK, svc
	})

	desired, err := parseManifest([]byte(validManifest))
	if err != nil {
		t.Fatalf("parseManifest() error = %v", err)
	}
	plan, err := c.PlanService(context.Background(), "my-svc", desired, PlanOptions{})
	if err != nil {
		t.Fatalf("PlanService() error = %v", err)
	}
	if got := plan.desired.Metadata.ResourceVersion; got != "from-server" {
		t.Errorf("resourceVersion = %q, want the one read from the API", got)
	}
}
