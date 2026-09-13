package cloudrun

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	run "google.golang.org/api/run/v1"
)

// defaultedService is the service definition after Cloud Run has filled in its defaults.
// It holds fields that validManifest does not have.
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

// defaultingAPI mimics a server that fills in defaults. It returns the definition with the
// defaults in it for a dryRun=all PUT, and returns the same thing for a GET too. It records the
// body of a PUT that is not a dryRun.
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
	// It is a minimal manifest, so whatever the server filled in becomes a diff as is (issue #11).
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

	// Resolving uses a dryRun=all write request (which changes nothing).
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

// TestPlanAppliesTheOriginalManifest checks that, even when the defaults are resolved, what is
// sent to apply is still the original manifest. Writing back the values the server filled in
// would pin them to the old values when Cloud Run's defaults change in the future.
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

// TestCompareManifestValidatesBeforeTheDryRun checks that, with --server-defaults, a service name
// mismatch is rejected before anything is sent to the API. A dry run returns 400 when the service
// name in the path and metadata.name in the body do not match, so sending it produces a confusing
// error that gets misread as "permission is required".
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

// TestCompareManifestSetsTheNamespace checks that the definition sent to the dry run goes through
// the same pre-processing as deploy (setting the namespace to the target). Without it, for a
// manifest that carries a namespace, only diff --server-defaults gets rejected by the API.
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

// TestCompareManifestTreatsAMissingServiceAsAnAddition checks that diff works against a service
// that has not been created yet. PlanService (deploy) treated a 404 as a create, but
// CompareManifest (diff) returned it as is, so diff failed on just the first run of the procedure
// the README recommends.
func TestCompareManifestTreatsAMissingServiceAsAnAddition(t *testing.T) {
	var mu sync.Mutex
	var dryRuns int
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if r.Method == http.MethodPost {
			// The service does not exist, so the dry run must be a Create.
			mu.Lock()
			dryRuns++
			mu.Unlock()
			return http.StatusOK, defaultedService()
		}
		return http.StatusNotFound, googleAPIError(404, "not found")
	})

	got, err := c.CompareManifest(context.Background(), "my-svc", []byte(validManifest),
		"manifest.yaml", PlanOptions{ResolveDefaults: true})
	if err != nil {
		t.Fatalf("CompareManifest() error = %v, want a missing service to be handled", err)
	}
	if !strings.Contains(got, "+kind: Service") {
		t.Errorf("CompareManifest() = %q, want the whole manifest shown as an addition", got)
	}
	mu.Lock()
	n := dryRuns
	mu.Unlock()
	if n != 1 {
		t.Errorf("dry-run Create count = %d, want 1 (a replace dry-run would 404 too)", n)
	}
}

func TestCompareManifestHandlesAMissingServiceWithoutResolvingDefaults(t *testing.T) {
	c, _ := newTestClient(t, nil) // the default handler returns 404

	got, err := c.CompareManifest(context.Background(), "my-svc", []byte(validManifest),
		"manifest.yaml", PlanOptions{})
	if err != nil {
		t.Fatalf("CompareManifest() error = %v", err)
	}
	if !strings.Contains(got, "+kind: Service") {
		t.Errorf("CompareManifest() = %q, want the whole manifest shown as an addition", got)
	}
}

// TestCompareManifestValidatesWithoutResolvingDefaults checks that, even with
// --no-server-defaults, the input goes through the same validation as deploy. Skipping it makes
// diff alone show "a diff as if the name could be changed" for a manifest deploy refuses.
func TestCompareManifestValidatesWithoutResolvingDefaults(t *testing.T) {
	c, recorded := defaultingAPI(t)

	_, err := c.CompareManifest(context.Background(), "other-svc", []byte(validManifest),
		"manifest.yaml", PlanOptions{})
	if err == nil {
		t.Fatal("CompareManifest() error = nil, want the name mismatch to be caught")
	}
	if !strings.Contains(err.Error(), "does not match service argument") {
		t.Errorf("CompareManifest() error = %v", err)
	}
	if n := len(recorded()); n != 0 {
		t.Errorf("requests = %d, want 0 (validation happens before any API call)", n)
	}
}

// TestResolveDefaultsErrorPointsAtTheFlag checks that, on insufficient permissions, the error says
// what to do. A dry run is a write, so read-only permissions do not get it through.
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
		"permission denied",    // shows the cause as is
		"dry-run update",       // the call that was actually made
		"--no-server-defaults", // points at the way out
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Plan() error = %v, want it to contain %q", err, want)
		}
	}
}

// TestResolveDefaultsErrorNamesTheCreatePath checks that, when a diff against a service that does
// not exist yet fails on insufficient permissions, the error points at the create permission. The
// dry run on this path is a Create, so it needs run.services.create. Pointing at update means
// someone adding the permission follows the advice and still does not get through.
func TestResolveDefaultsErrorNamesTheCreatePath(t *testing.T) {
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		switch r.Method {
		case http.MethodGet:
			return http.StatusNotFound, googleAPIError(404, "not found")
		case http.MethodPost:
			return http.StatusForbidden, googleAPIError(403, "permission denied")
		}
		return http.StatusOK, defaultedService()
	})

	_, err := c.CompareManifest(context.Background(), "my-svc", []byte(validManifest),
		"local/manifest.yaml", PlanOptions{ResolveDefaults: true})
	if err == nil {
		t.Fatal("CompareManifest() error = nil, want the permission failure to surface")
	}
	for _, want := range []string{
		"failed to resolve server defaults",
		"permission denied",
		"dry-run create",       // not update
		"permission to create", // the permission to add
		"--no-server-defaults",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("CompareManifest() error = %v, want it to contain %q", err, want)
		}
	}
}
