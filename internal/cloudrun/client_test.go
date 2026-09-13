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

// recordedRequest is a record of a request the fake API received.
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Body   []byte
}

// fakeAPI is an httptest server that stands in for the Cloud Run Admin API.
// ServeHTTP runs on the server's goroutine, so the records are guarded by mu and failures are
// reported with Errorf (the FailNow family may only be called from the test's own goroutine).
type fakeAPI struct {
	t *testing.T
	// handler decides the response to each request. When nil, it returns 404.
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

// recorded returns a copy of the recorded requests.
func (f *fakeAPI) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

// googleAPIError builds the JSON of a Google API error response.
func googleAPIError(code int, message string) map[string]interface{} {
	return map[string]interface{}{
		"error": map[string]interface{}{"code": code, "message": message, "status": "NOT_FOUND"},
	}
}

// newTestClient returns a Client pointed at the httptest fake API. It does not use ADC, so it
// works in an environment with no credentials.
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

// liveService is the live service definition the fake API returns.
func liveService(image string) *run.Service {
	return &run.Service{
		ApiVersion: manifestAPIVersion,
		Kind:       manifestKind,
		Metadata: &run.ObjectMeta{
			Name:      "my-svc",
			Namespace: testProject,
			// Server-managed fields. They are dropped when converting to a manifest.
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
	c, _ := newTestClient(t, nil) // the default handler returns 404

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
	c, _ := newTestClient(t, nil) // GET returns 404

	plan, err := c.Plan(context.Background(), "my-svc", []byte(validManifest), PlanOptions{})
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

	plan, err := c.Plan(context.Background(), "my-svc", []byte(validManifest), PlanOptions{})
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
	// Server-managed fields (uid/generation/status) must not appear in the diff.
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

	plan, err := c.Plan(context.Background(), "my-svc", []byte(validManifest), PlanOptions{})
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

	_, err := c.Plan(context.Background(), "other-svc", []byte(validManifest), PlanOptions{})
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

	plan, err := c.Plan(context.Background(), "my-svc", []byte(validManifest), PlanOptions{})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	applied, err := plan.Apply(context.Background(), false)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	// It returns the service as applied (Wait uses it to learn the generation).
	if applied == nil || applied.Metadata == nil || applied.Metadata.Name != "my-svc" {
		t.Errorf("Apply() = %+v, want the applied service", applied)
	}

	post := lastRequest(t, api, http.MethodPost)
	wantPath := "/apis/serving.knative.dev/v1/namespaces/test-project/services"
	if post.Path != wantPath {
		t.Errorf("Create path = %q, want %q", post.Path, wantPath)
	}
	// When dryRun is false, the dryRun parameter is not sent at all.
	if strings.Contains(post.Query, "dryRun") {
		t.Errorf("Create query = %q, want no dryRun parameter", post.Query)
	}
	// The namespace in the sent body is set to the project.
	if !strings.Contains(string(post.Body), `"namespace":"test-project"`) {
		t.Errorf("Create body = %s, want namespace set to the project", post.Body)
	}
}

func TestApplyReplaceWithDryRun(t *testing.T) {
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		return http.StatusOK, liveService("gcr.io/project/image:old")
	})

	plan, err := c.Plan(context.Background(), "my-svc", []byte(validManifest), PlanOptions{})
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

	plan, err := c.Plan(context.Background(), "my-svc", []byte(validManifest), PlanOptions{})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	_, err = plan.Apply(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), `failed to update service "my-svc"`) {
		t.Fatalf("Apply() error = %v, want it to name the service", err)
	}
}

func TestDeleteService(t *testing.T) {
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		return http.StatusOK, map[string]interface{}{}
	})

	if err := c.DeleteService(context.Background(), "my-svc", false); err != nil {
		t.Fatalf("DeleteService() error = %v", err)
	}

	del := lastRequest(t, api, http.MethodDelete)
	wantPath := "/apis/serving.knative.dev/v1/namespaces/test-project/services/my-svc"
	if del.Path != wantPath {
		t.Errorf("Delete path = %q, want %q", del.Path, wantPath)
	}
	if strings.Contains(del.Query, "dryRun") {
		t.Errorf("Delete query = %q, want no dryRun parameter", del.Query)
	}
}

func TestDeleteServiceDryRun(t *testing.T) {
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		return http.StatusOK, map[string]interface{}{}
	})

	if err := c.DeleteService(context.Background(), "my-svc", true); err != nil {
		t.Fatalf("DeleteService() error = %v", err)
	}
	if del := lastRequest(t, api, http.MethodDelete); !strings.Contains(del.Query, "dryRun=all") {
		t.Errorf("Delete query = %q, want it to contain dryRun=all", del.Query)
	}
}

func TestDeleteServicePropagatesErrors(t *testing.T) {
	c, _ := newTestClient(t, nil) // the default handler returns 404

	err := c.DeleteService(context.Background(), "missing", false)
	if err == nil || !strings.Contains(err.Error(), `failed to delete service "missing"`) {
		t.Fatalf("DeleteService() error = %v, want it to name the service", err)
	}
}

// lastRequest returns the last request received with the given method.
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

// TestApplySendsTheResourceVersionItComparedAgainst checks that an update write carries the
// resourceVersion of "the service the diff was computed against". Sent without it, Cloud Run
// accepts the write as an unconditional overwrite (verified against the real API), so concurrent
// deploys silently erase each other's changes.
func TestApplySendsTheResourceVersionItComparedAgainst(t *testing.T) {
	const liveRV = "AAZZrzudm44"
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		live := liveService("gcr.io/project/image:old")
		live.Metadata.ResourceVersion = liveRV
		return http.StatusOK, live
	})

	plan, err := c.Plan(context.Background(), "my-svc", []byte(validManifest), PlanOptions{})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if _, err := plan.Apply(context.Background(), false); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	put := lastRequest(t, api, http.MethodPut)
	var sent run.Service
	if err := json.Unmarshal(put.Body, &sent); err != nil {
		t.Fatalf("failed to parse the request body: %v", err)
	}
	if sent.Metadata == nil {
		t.Fatal("ReplaceService sent no metadata")
	}
	if sent.Metadata.ResourceVersion != liveRV {
		t.Errorf("ReplaceService sent resourceVersion = %q, want %q", sent.Metadata.ResourceVersion, liveRV)
	}
}

// TestApplyExplainsAConcurrentChange checks that a 409 is explained as "another change landed
// after the diff was computed". The API's wording alone (version 'X' was specified but current
// version is 'Y') does not tell the user what to do.
func TestApplyExplainsAConcurrentChange(t *testing.T) {
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if r.Method == http.MethodPut {
			return http.StatusConflict, googleAPIError(409,
				"Conflict for resource 'my-svc': version '1' was specified but current version is '2'.")
		}
		return http.StatusOK, liveService("gcr.io/project/image:old")
	})

	plan, err := c.Plan(context.Background(), "my-svc", []byte(validManifest), PlanOptions{})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	_, err = plan.Apply(context.Background(), false)
	if err == nil {
		t.Fatal("Apply() error = nil, want a conflict")
	}
	if !strings.Contains(err.Error(), "changed after the diff was computed") {
		t.Errorf("Apply() error = %v, want it to explain the concurrent change", err)
	}
	// The original API error is kept too (which version the conflict was on is needed to
	// investigate).
	if !strings.Contains(err.Error(), "current version is '2'") {
		t.Errorf("Apply() error = %v, want it to keep the API message", err)
	}
}

// TestPlanCreateSendsNoResourceVersion checks that a create carries no resourceVersion. Cloud
// Run returns 400 for a malformed resourceVersion, so putting anything there for a service that
// does not exist breaks the create itself.
func TestPlanCreateSendsNoResourceVersion(t *testing.T) {
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if r.Method == http.MethodGet {
			return http.StatusNotFound, googleAPIError(404, "not found")
		}
		return http.StatusOK, liveService("gcr.io/project/image:new")
	})

	plan, err := c.Plan(context.Background(), "my-svc", []byte(validManifest), PlanOptions{})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if !plan.Create {
		t.Fatalf("Plan() Create = false, want a create")
	}
	if _, err := plan.Apply(context.Background(), false); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	post := lastRequest(t, api, http.MethodPost)
	if strings.Contains(string(post.Body), "resourceVersion") {
		t.Errorf("Create body should not carry a resourceVersion:\n%s", post.Body)
	}
}
