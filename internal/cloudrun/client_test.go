package cloudrun

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/api/option"
	run "google.golang.org/api/run/v1"
)

const (
	testProject = "test-project"
	testRegion  = "asia-northeast1"
)

// recordedRequest はフェイク API が受け取ったリクエストの記録。
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Body   []byte
}

// fakeAPI は Cloud Run Admin API の代わりに使う httptest サーバ。
// ServeHTTP はサーバの goroutine で走るので、記録は mu で保護し、失敗は Errorf で
// 報告する (FailNow 系はテスト本体の goroutine からしか呼べない)。
type fakeAPI struct {
	t *testing.T
	// handler はリクエストごとの応答を決める。nil なら 404 を返す。
	handler func(r *http.Request) (status int, body interface{})

	mu       sync.Mutex
	requests []recordedRequest
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("failed to read the request body: %v", err)
	}
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: body,
	})
	f.mu.Unlock()

	status, payload := http.StatusNotFound, interface{}(googleAPIError(404, "not found"))
	if f.handler != nil {
		status, payload = f.handler(r)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		f.t.Errorf("failed to encode the fake response: %v", err)
	}
}

// recorded は記録済みリクエストのコピーを返す。
func (f *fakeAPI) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

// googleAPIError は Google API のエラー応答 JSON を組み立てる。
func googleAPIError(code int, message string) map[string]interface{} {
	return map[string]interface{}{
		"error": map[string]interface{}{"code": code, "message": message, "status": "NOT_FOUND"},
	}
}

// newTestClient は httptest のフェイク API に向いた Client を返す。ADC は使わないので
// 認証情報の無い環境でも動く。
func newTestClient(t *testing.T, handler func(r *http.Request) (int, interface{})) (*Client, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{t: t, handler: handler}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	c, err := NewClient(context.Background(), testProject, testRegion,
		option.WithEndpoint(srv.URL+"/"), option.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return c, api
}

// liveService はフェイク API が返す live サービス定義。
func liveService(image string) *run.Service {
	return &run.Service{
		ApiVersion: manifestAPIVersion,
		Kind:       manifestKind,
		Metadata: &run.ObjectMeta{
			Name:      "my-svc",
			Namespace: testProject,
			// サーバ管理フィールド。マニフェスト化の際に落とされる。
			Uid:        "abc-123",
			Generation: 7,
		},
		Spec: &run.ServiceSpec{
			Template: &run.RevisionTemplate{
				Spec: &run.RevisionSpec{
					Containers: []*run.Container{{Image: image}},
				},
			},
		},
		Status: &run.ServiceStatus{LatestReadyRevisionName: "my-svc-00007-abc"},
	}
}

func TestNewClientRequiresProjectAndRegion(t *testing.T) {
	tests := []struct {
		name            string
		project, region string
		wantErr         string
	}{
		{name: "no project", region: testRegion, wantErr: "project is required"},
		{name: "no region", project: testProject, wantErr: "region is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewClient(context.Background(), tt.project, tt.region)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("NewClient() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestRegionalEndpoint(t *testing.T) {
	if got, want := regionalEndpoint("us-central1"), "https://us-central1-run.googleapis.com"; got != want {
		t.Errorf("regionalEndpoint() = %q, want %q", got, want)
	}
}

func TestGetService(t *testing.T) {
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		return http.StatusOK, liveService("gcr.io/p/img:v1")
	})

	obj, err := c.GetService(context.Background(), "my-svc")
	if err != nil {
		t.Fatalf("GetService() error = %v", err)
	}
	if obj.Metadata.Name != "my-svc" {
		t.Errorf("GetService() name = %q, want %q", obj.Metadata.Name, "my-svc")
	}

	got := api.recorded()
	wantPath := "/apis/serving.knative.dev/v1/namespaces/test-project/services/my-svc"
	if len(got) != 1 || got[0].Path != wantPath {
		t.Fatalf("requests = %+v, want a single GET to %q", got, wantPath)
	}
	if got[0].Method != http.MethodGet {
		t.Errorf("method = %q, want GET", got[0].Method)
	}
}

func TestGetServiceNotFound(t *testing.T) {
	c, _ := newTestClient(t, nil) // 既定の handler は 404

	_, err := c.GetService(context.Background(), "missing")
	if err == nil {
		t.Fatal("GetService() error = nil, want an error")
	}
	if !isNotFound(err) {
		t.Errorf("isNotFound(%v) = false, want true", err)
	}
	if !strings.Contains(err.Error(), `failed to get service "missing"`) {
		t.Errorf("GetService() error = %v, want it to name the service", err)
	}
}

func TestPlanCreatesWhenServiceIsMissing(t *testing.T) {
	c, _ := newTestClient(t, nil) // GET は 404

	plan, err := c.Plan(context.Background(), "my-svc", []byte(validManifest))
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if !plan.Create {
		t.Error("Plan().Create = false, want true for a missing service")
	}
	if !strings.Contains(plan.Diff, "+kind: Service") {
		t.Errorf("Plan().Diff = %q, want the whole manifest added", plan.Diff)
	}
}

func TestPlanDiffsAgainstLiveService(t *testing.T) {
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		return http.StatusOK, liveService("gcr.io/project/image:old")
	})

	plan, err := c.Plan(context.Background(), "my-svc", []byte(validManifest))
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if plan.Create {
		t.Error("Plan().Create = true, want false for an existing service")
	}
	if !strings.Contains(plan.Diff, "-      - image: gcr.io/project/image:old") ||
		!strings.Contains(plan.Diff, "+      - image: gcr.io/project/image:tag") {
		t.Errorf("Plan().Diff = %q, want the image change", plan.Diff)
	}
	// サーバ管理フィールド (uid/generation/status) は diff に出てはいけない。
	for _, field := range []string{"uid", "generation", "status"} {
		if strings.Contains(plan.Diff, field+":") {
			t.Errorf("Plan().Diff contains the server-managed field %q:\n%s", field, plan.Diff)
		}
	}
}

func TestPlanNoDiffWhenIdentical(t *testing.T) {
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		return http.StatusOK, liveService("gcr.io/project/image:tag")
	})

	plan, err := c.Plan(context.Background(), "my-svc", []byte(validManifest))
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if plan.Diff != "" {
		t.Errorf("Plan().Diff = %q, want empty", plan.Diff)
	}
}

func TestPlanRejectsInvalidManifestBeforeCallingTheAPI(t *testing.T) {
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		return http.StatusOK, liveService("gcr.io/p/img:v1")
	})

	_, err := c.Plan(context.Background(), "other-svc", []byte(validManifest))
	if err == nil {
		t.Fatal("Plan() error = nil, want a name mismatch error")
	}
	if got := api.recorded(); len(got) != 0 {
		t.Errorf("requests = %+v, want none (validation happens before the API call)", got)
	}
}

func TestApplyCreate(t *testing.T) {
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if r.Method == http.MethodPost {
			return http.StatusOK, liveService("gcr.io/project/image:tag")
		}
		return http.StatusNotFound, googleAPIError(404, "not found")
	})

	plan, err := c.Plan(context.Background(), "my-svc", []byte(validManifest))
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	applied, err := plan.Apply(context.Background(), false)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	// 適用後のサービスを返す (Wait が世代を知るために使う)。
	if applied == nil || applied.Metadata == nil || applied.Metadata.Name != "my-svc" {
		t.Errorf("Apply() = %+v, want the applied service", applied)
	}

	post := lastRequest(t, api, http.MethodPost)
	wantPath := "/apis/serving.knative.dev/v1/namespaces/test-project/services"
	if post.Path != wantPath {
		t.Errorf("Create path = %q, want %q", post.Path, wantPath)
	}
	// dryRun が false のときは dryRun パラメータ自体を送らない。
	if strings.Contains(post.Query, "dryRun") {
		t.Errorf("Create query = %q, want no dryRun parameter", post.Query)
	}
	// 送信 body の namespace はプロジェクトに揃えられている。
	if !strings.Contains(string(post.Body), `"namespace":"test-project"`) {
		t.Errorf("Create body = %s, want namespace set to the project", post.Body)
	}
}

func TestApplyReplaceWithDryRun(t *testing.T) {
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		return http.StatusOK, liveService("gcr.io/project/image:old")
	})

	plan, err := c.Plan(context.Background(), "my-svc", []byte(validManifest))
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if _, err := plan.Apply(context.Background(), true); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	put := lastRequest(t, api, http.MethodPut)
	wantPath := "/apis/serving.knative.dev/v1/namespaces/test-project/services/my-svc"
	if put.Path != wantPath {
		t.Errorf("ReplaceService path = %q, want %q", put.Path, wantPath)
	}
	if !strings.Contains(put.Query, "dryRun=all") {
		t.Errorf("ReplaceService query = %q, want it to contain dryRun=all", put.Query)
	}
}

func TestApplyReportsServerErrors(t *testing.T) {
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if r.Method == http.MethodPut {
			return http.StatusForbidden, googleAPIError(403, "permission denied")
		}
		return http.StatusOK, liveService("gcr.io/project/image:old")
	})

	plan, err := c.Plan(context.Background(), "my-svc", []byte(validManifest))
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	_, err = plan.Apply(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), `failed to update service "my-svc"`) {
		t.Fatalf("Apply() error = %v, want it to name the service", err)
	}
}

// lastRequest は指定メソッドで最後に受け取ったリクエストを返す。
func lastRequest(t *testing.T, api *fakeAPI, method string) recordedRequest {
	t.Helper()
	got := api.recorded()
	for i := len(got) - 1; i >= 0; i-- {
		if got[i].Method == method {
			return got[i]
		}
	}
	t.Fatalf("no %s request was recorded, got %+v", method, got)
	return recordedRequest{}
}
