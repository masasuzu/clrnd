package cloudrun

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"google.golang.org/api/option"
	run "google.golang.org/api/run/v1"
)

func TestSecretResourceName(t *testing.T) {
	aliases := map[string]string{
		"db_pass":    "projects/other-proj/secrets/db-password",
		"with_ver":   "projects/other-proj/secrets/api-key/versions/3",
		"short_only": "shorthand", // error case: no projects/ prefix
	}
	tests := []struct {
		name    string
		project string
		secret  string
		want    string
	}{
		{"same-project short name", "p", "my-secret", "projects/p/secrets/my-secret"},
		{"already qualified", "p", "projects/q/secrets/s", "projects/q/secrets/s"},
		{"qualified with version stripped", "p", "projects/q/secrets/s/versions/5", "projects/q/secrets/s"},
		{"cross-project alias resolved", "p", "db_pass", "projects/other-proj/secrets/db-password"},
		{"cross-project alias with version stripped", "p", "with_ver", "projects/other-proj/secrets/api-key"},
		{"unknown alias falls back to same project", "p", "missing", "projects/p/secrets/missing"},
		{"alias target not qualified falls back", "p", "short_only", "projects/p/secrets/shorthand"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := secretResourceName(tt.project, tt.secret, aliases); got != tt.want {
				t.Errorf("secretResourceName(%q, %q) = %q, want %q", tt.project, tt.secret, got, tt.want)
			}
		})
	}
}

func TestSecretAliases(t *testing.T) {
	t.Run("nil-safe on empty service", func(t *testing.T) {
		if got := secretAliases(&run.Service{}); got != nil {
			t.Errorf("secretAliases(empty) = %v, want nil", got)
		}
	})

	t.Run("parses comma-separated aliases", func(t *testing.T) {
		svc := &run.Service{
			Spec: &run.ServiceSpec{
				Template: &run.RevisionTemplate{
					Metadata: &run.ObjectMeta{
						Annotations: map[string]string{
							secretAliasAnnotation: "a:projects/p1/secrets/s1, b:projects/p2/secrets/s2",
						},
					},
				},
			},
		}
		got := secretAliases(svc)
		want := map[string]string{
			"a": "projects/p1/secrets/s1",
			"b": "projects/p2/secrets/s2",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("secretAliases() = %v, want %v", got, want)
		}
	})
}

func TestSecretNames(t *testing.T) {
	svc := &run.Service{
		Spec: &run.ServiceSpec{
			Template: &run.RevisionTemplate{
				Spec: &run.RevisionSpec{
					Containers: []*run.Container{{
						Env: []*run.EnvVar{
							{Name: "A", Value: "plain"},
							{Name: "B", ValueFrom: &run.EnvVarSource{SecretKeyRef: &run.SecretKeySelector{Name: "s1", Key: "latest"}}},
							{Name: "C", ValueFrom: &run.EnvVarSource{SecretKeyRef: &run.SecretKeySelector{Name: "s1", Key: "1"}}}, // duplicate
						},
					}},
					Volumes: []*run.Volume{
						{Name: "v", Secret: &run.SecretVolumeSource{SecretName: "s2"}},
					},
				},
			},
		},
	}
	got := secretNames(svc)
	want := []string{"s1", "s2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("secretNames() = %v, want %v (deduped, env + volume)", got, want)
	}
}

// --- VerifyRemote's remote path ---

// verifyManifest is a manifest that references a runtime service account and a secret.
const verifyManifest = `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      serviceAccountName: runner@other-project.iam.gserviceaccount.com
      containers:
      - image: gcr.io/project/image:tag
        env:
        - name: TOKEN
          valueFrom:
            secretKeyRef:
              name: api-token
              key: latest
`

// startVerifyAPI starts a fake API that answers every API VerifyRemote calls, with the status that
// status returns for each path, and records the request paths it receives. Each of those clients
// uses the same endpoint option, so a single server routes them by path.
func startVerifyAPI(t *testing.T, status func(path string) int) (func() []string, []option.ClientOption) {
	t.Helper()
	var mu sync.Mutex
	var paths []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()

		code := status(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if code == http.StatusOK {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"error": {"code": %d, "message": "boom"}}`, code)
	}))
	t.Cleanup(srv.Close)

	recorded := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
	return recorded, []option.ClientOption{
		option.WithEndpoint(srv.URL + "/"),
		option.WithHTTPClient(srv.Client()),
	}
}

// TestVerifyRemoteLooksUpTheServiceAccountAcrossProjects checks that the runtime service account
// is looked up independently of the project. Cloud Run can use a service account from another
// project as the runtime service account, so pinning the project being verified turns a valid
// setup into a 404 = Missing.
func TestVerifyRemoteLooksUpTheServiceAccountAcrossProjects(t *testing.T) {
	recorded, opts := startVerifyAPI(t, func(string) int { return http.StatusOK })

	res, err := VerifyRemote(context.Background(), testProject, testRegion, []byte(verifyManifest), opts...)
	if err != nil {
		t.Fatalf("VerifyRemote() error = %v", err)
	}
	if len(res.Missing) != 0 || len(res.Unchecked) != 0 {
		t.Fatalf("VerifyRemote() = %+v, want everything to check out", res)
	}

	var sawSA bool
	for _, p := range recorded() {
		if strings.Contains(p, "/serviceAccounts/") {
			sawSA = true
			if !strings.Contains(p, "projects/-/serviceAccounts/") {
				t.Errorf("service account lookup path = %q, want the project wildcard so that a "+
					"service account from another project resolves", p)
			}
		}
	}
	if !sawSA {
		t.Error("no service account lookup was made")
	}
}

func TestVerifyRemoteReportsMissingResources(t *testing.T) {
	recorded, opts := startVerifyAPI(t, func(string) int { return http.StatusNotFound })

	res, err := VerifyRemote(context.Background(), testProject, testRegion, []byte(verifyManifest), opts...)
	if err != nil {
		t.Fatalf("VerifyRemote() error = %v", err)
	}
	if len(res.Missing) != 2 {
		t.Fatalf("Missing = %v, want the service account and the secret", res.Missing)
	}
	if len(res.Unchecked) != 0 {
		t.Errorf("Unchecked = %v, want empty (404 is a decision, not an unknown)", res.Unchecked)
	}
	if len(recorded()) != 2 {
		t.Errorf("requests = %v, want one for the service account and one for the secret", recorded())
	}
}

// TestVerifyRemoteTreatsOtherFailuresAsUnchecked checks that insufficient permissions and the like
// are sorted into Unchecked rather than Missing. Making them a failure would break the offline lint
// of a CI that merely has an ambient project/region.
func TestVerifyRemoteTreatsOtherFailuresAsUnchecked(t *testing.T) {
	_, opts := startVerifyAPI(t, func(string) int { return http.StatusForbidden })

	res, err := VerifyRemote(context.Background(), testProject, testRegion, []byte(verifyManifest), opts...)
	if err != nil {
		t.Fatalf("VerifyRemote() error = %v", err)
	}
	if len(res.Missing) != 0 {
		t.Errorf("Missing = %v, want empty (permission denied does not prove absence)", res.Missing)
	}
	if len(res.Unchecked) != 2 {
		t.Errorf("Unchecked = %v, want both resources reported as undecidable", res.Unchecked)
	}
}

// TestVerifyRemoteSkipsWhatTheManifestDoesNotReference checks that the API is not called when
// nothing is referenced.
func TestVerifyRemoteSkipsWhatTheManifestDoesNotReference(t *testing.T) {
	recorded, opts := startVerifyAPI(t, func(string) int { return http.StatusOK })

	res, err := VerifyRemote(context.Background(), testProject, testRegion, []byte(validManifest), opts...)
	if err != nil {
		t.Fatalf("VerifyRemote() error = %v", err)
	}
	if len(res.Missing) != 0 || len(res.Unchecked) != 0 {
		t.Errorf("VerifyRemote() = %+v, want nothing to report", res)
	}
	if n := len(recorded()); n != 0 {
		t.Errorf("requests = %d, want 0 (the manifest references no service account or secret)", n)
	}
}

// arManifest is a manifest that references Artifact Registry images (one by tag, and one by digest
// with a nested path). It has neither a service account nor a secret, so only the images are
// checked.
const arManifest = `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      - image: asia-northeast1-docker.pkg.dev/img-project/repo/app:v1
      - image: us-docker.pkg.dev/img-project/repo/team/side@sha256:abc123
`

// TestVerifyRemoteChecksArtifactRegistryImages checks that the image existence check looks up the
// right resource for a tag reference and for a digest reference respectively.
// Both the location and the project come from the image reference, so an image in another project
// goes through as is (treated the same as the runtime service account).
func TestVerifyRemoteChecksArtifactRegistryImages(t *testing.T) {
	recorded, opts := startVerifyAPI(t, func(string) int { return http.StatusOK })

	res, err := VerifyRemote(context.Background(), testProject, testRegion, []byte(arManifest), opts...)
	if err != nil {
		t.Fatalf("VerifyRemote() error = %v", err)
	}
	if len(res.Missing) > 0 || len(res.Unchecked) > 0 {
		t.Fatalf("VerifyRemote() = %+v, want everything to check out", res)
	}

	paths := recorded()
	want := []string{
		"/v1/projects/img-project/locations/asia-northeast1/repositories/repo/packages/app/tags/v1",
		"/v1/projects/img-project/locations/us/repositories/repo/dockerImages/team%2Fside@sha256:abc123",
	}
	for _, w := range want {
		if !containsPath(paths, w) {
			t.Errorf("requested %v, want it to include %q", paths, w)
		}
	}
}

// TestVerifyRemoteReportsAMissingImage checks that only a 404 becomes Missing.
func TestVerifyRemoteReportsAMissingImage(t *testing.T) {
	_, opts := startVerifyAPI(t, func(path string) int {
		if strings.Contains(path, "/packages/") {
			return http.StatusNotFound
		}
		return http.StatusOK
	})

	res, err := VerifyRemote(context.Background(), testProject, testRegion, []byte(arManifest), opts...)
	if err != nil {
		t.Fatalf("VerifyRemote() error = %v", err)
	}
	if len(res.Missing) != 1 || !strings.Contains(res.Missing[0], "app:v1") {
		t.Errorf("Missing = %v, want the tagged image reported as absent", res.Missing)
	}
	if len(res.Unchecked) != 0 {
		t.Errorf("Unchecked = %v, want empty (404 is a decision, not an unknown)", res.Unchecked)
	}
}

// TestVerifyRemoteTreatsAnInaccessibleImageAsUnchecked checks that a 403 is not made Missing. On
// the real API a project that does not exist (or cannot be accessed) returns 403, so making it
// Missing here fails verify on a valid setup.
func TestVerifyRemoteTreatsAnInaccessibleImageAsUnchecked(t *testing.T) {
	_, opts := startVerifyAPI(t, func(string) int { return http.StatusForbidden })

	res, err := VerifyRemote(context.Background(), testProject, testRegion, []byte(arManifest), opts...)
	if err != nil {
		t.Fatalf("VerifyRemote() error = %v", err)
	}
	if len(res.Missing) != 0 {
		t.Errorf("Missing = %v, want empty (403 does not prove absence)", res.Missing)
	}
	if len(res.Unchecked) != 2 {
		t.Errorf("Unchecked = %v, want both images reported as undecidable", res.Unchecked)
	}
}

// TestVerifyRemoteSkipsRegistriesItCannotCheck checks that nothing is said about registries that
// cannot be checked. Making this a warning would print a warning on every run just for using a
// Docker Hub image, and people would start skipping over the warnings themselves.
func TestVerifyRemoteSkipsRegistriesItCannotCheck(t *testing.T) {
	recorded, opts := startVerifyAPI(t, func(string) int { return http.StatusOK })

	// validManifest's image is on gcr.io.
	res, err := VerifyRemote(context.Background(), testProject, testRegion, []byte(validManifest), opts...)
	if err != nil {
		t.Fatalf("VerifyRemote() error = %v", err)
	}
	if len(res.Missing) != 0 || len(res.Unchecked) != 0 {
		t.Errorf("VerifyRemote() = %+v, want nothing to report", res)
	}
	for _, p := range recorded() {
		if strings.Contains(p, "/repositories/") {
			t.Errorf("requested %q, want no Artifact Registry call for a gcr.io image", p)
		}
	}
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// --- VPC connector / Cloud SQL / secret versions ---

// verifyRefsManifest is a manifest that references a VPC connector (by short name) and Cloud SQL.
// Its secret specifies the version explicitly.
const verifyRefsManifest = `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    metadata:
      annotations:
        run.googleapis.com/vpc-access-connector: my-connector
        run.googleapis.com/cloudsql-instances: other-project:asia-northeast1:main-db
    spec:
      containers:
      - image: gcr.io/project/image:tag
        env:
        - name: TOKEN
          valueFrom:
            secretKeyRef:
              name: api-token
              key: "3"
`

// pathsMatching returns the recorded requests that contain the substring.
func pathsMatching(paths []string, substr string) []string {
	var out []string
	for _, p := range paths {
		if strings.Contains(p, substr) {
			out = append(out, p)
		}
	}
	return out
}

// TestVerifyRemoteChecksTheVPCConnector checks that a connector given by short name is completed
// into a full resource name using the deploy target's project and region. A connector is a
// regional resource, so this is the only place the region is needed.
func TestVerifyRemoteChecksTheVPCConnector(t *testing.T) {
	recorded, opts := startVerifyAPI(t, func(string) int { return http.StatusOK })

	if _, err := VerifyRemote(context.Background(), testProject, testRegion,
		[]byte(verifyRefsManifest), opts...); err != nil {
		t.Fatalf("VerifyRemote() error = %v", err)
	}

	got := pathsMatching(recorded(), "/connectors/")
	if len(got) != 1 {
		t.Fatalf("connector lookups = %v, want exactly one", got)
	}
	want := fmt.Sprintf("projects/%s/locations/%s/connectors/my-connector", testProject, testRegion)
	if !strings.Contains(got[0], want) {
		t.Errorf("connector lookup path = %q, want it to contain %q", got[0], want)
	}
}

// TestVerifyRemoteKeepsAFullyQualifiedConnector checks that, when written as a full resource name,
// it is used as written (a connector in another project or region is not overwritten with the
// deploy target's values).
func TestVerifyRemoteKeepsAFullyQualifiedConnector(t *testing.T) {
	manifest := strings.Replace(verifyRefsManifest, "vpc-access-connector: my-connector",
		"vpc-access-connector: projects/other-project/locations/us-central1/connectors/shared", 1)
	recorded, opts := startVerifyAPI(t, func(string) int { return http.StatusOK })

	if _, err := VerifyRemote(context.Background(), testProject, testRegion, []byte(manifest), opts...); err != nil {
		t.Fatalf("VerifyRemote() error = %v", err)
	}

	got := pathsMatching(recorded(), "/connectors/")
	if len(got) != 1 || !strings.Contains(got[0], "projects/other-project/locations/us-central1/connectors/shared") {
		t.Errorf("connector lookups = %v, want the qualified name used as written", got)
	}
}

// TestVerifyRemoteReportsMissingReferences checks that references that come back 404 become
// Missing. Each of them is the kind of reference that only fails once you deploy.
func TestVerifyRemoteReportsMissingReferences(t *testing.T) {
	recorded, opts := startVerifyAPI(t, func(path string) int {
		// Set up a situation where the secret itself exists but only the specified version does
		// not.
		if strings.Contains(path, "/versions/") ||
			strings.Contains(path, "/connectors/") ||
			strings.Contains(path, "/instances/") {
			return http.StatusNotFound
		}
		return http.StatusOK
	})

	res, err := VerifyRemote(context.Background(), testProject, testRegion,
		[]byte(verifyRefsManifest), opts...)
	if err != nil {
		t.Fatalf("VerifyRemote() error = %v", err)
	}
	if len(res.Unchecked) != 0 {
		t.Errorf("Unchecked = %v, want empty (404 is a decision)", res.Unchecked)
	}
	joined := strings.Join(res.Missing, "\n")
	for _, want := range []string{
		`secret "api-token" has no version "3"`,
		`VPC connector "my-connector" does not exist`,
		`Cloud SQL instance "other-project:asia-northeast1:main-db" does not exist`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("Missing = %v, want it to contain %q", res.Missing, want)
		}
	}
	// Cloud SQL is looked up in the connection name's project (not pinned to the deploy target).
	if got := pathsMatching(recorded(), "/instances/"); len(got) != 1 ||
		!strings.Contains(got[0], "other-project") {
		t.Errorf("Cloud SQL lookups = %v, want the project from the connection name", got)
	}
}

// TestVerifyRemoteSkipsVersionsOfAMissingSecret checks that, when the secret itself does not
// exist, its version is not queried. Listing the two side by side tells nothing more, and buries
// the real cause.
func TestVerifyRemoteSkipsVersionsOfAMissingSecret(t *testing.T) {
	recorded, opts := startVerifyAPI(t, func(path string) int {
		if strings.Contains(path, "/secrets/") {
			return http.StatusNotFound
		}
		return http.StatusOK
	})

	res, err := VerifyRemote(context.Background(), testProject, testRegion,
		[]byte(verifyRefsManifest), opts...)
	if err != nil {
		t.Fatalf("VerifyRemote() error = %v", err)
	}
	if got := pathsMatching(recorded(), "/versions/"); len(got) != 0 {
		t.Errorf("version lookups = %v, want none once the secret is known to be missing", got)
	}
	for _, m := range res.Missing {
		if strings.Contains(m, "version") {
			t.Errorf("Missing = %v, want no version entry for a missing secret", res.Missing)
		}
	}
}

// TestVerifyRemoteReportsAMalformedCloudSQLConnection checks that a connection name of the wrong
// shape falls on the side of "could not be checked" rather than "does not exist".
func TestVerifyRemoteReportsAMalformedCloudSQLConnection(t *testing.T) {
	manifest := strings.Replace(verifyRefsManifest,
		"cloudsql-instances: other-project:asia-northeast1:main-db",
		"cloudsql-instances: main-db", 1)
	recorded, opts := startVerifyAPI(t, func(string) int { return http.StatusOK })

	res, err := VerifyRemote(context.Background(), testProject, testRegion, []byte(manifest), opts...)
	if err != nil {
		t.Fatalf("VerifyRemote() error = %v", err)
	}
	if len(res.Missing) != 0 {
		t.Errorf("Missing = %v, want a malformed value not to be reported as absent", res.Missing)
	}
	if len(pathsMatching(recorded(), "/instances/")) != 0 {
		t.Error("a Cloud SQL lookup was made for a value that could not be parsed")
	}
	if len(res.Unchecked) != 1 || !strings.Contains(res.Unchecked[0], "main-db") {
		t.Errorf("Unchecked = %v, want the malformed connection reported", res.Unchecked)
	}
}

// TestVerifyRemoteSkipsUnreferencedAPIs checks that, for a manifest without the annotations, the
// VPC / Cloud SQL APIs are not touched. verify does not require enabling APIs that are not in use.
func TestVerifyRemoteSkipsUnreferencedAPIs(t *testing.T) {
	recorded, opts := startVerifyAPI(t, func(string) int { return http.StatusOK })

	if _, err := VerifyRemote(context.Background(), testProject, testRegion,
		[]byte(verifyManifest), opts...); err != nil {
		t.Fatalf("VerifyRemote() error = %v", err)
	}
	for _, substr := range []string{"/connectors/", "/instances/"} {
		if got := pathsMatching(recorded(), substr); len(got) != 0 {
			t.Errorf("lookups matching %q = %v, want none", substr, got)
		}
	}
}

// TestVerifyRemoteReportsADestroyedSecretVersion checks that destroyed and disabled versions are
// treated the same as versions that do not exist. Secret Manager returns these with a 200 on get
// too (it is access that stops working), so without looking at the state they slip through.
func TestVerifyRemoteReportsADestroyedSecretVersion(t *testing.T) {
	for _, state := range []string{"DESTROYED", "DISABLED"} {
		t.Run(state, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "/versions/") {
					_, _ = fmt.Fprintf(w, `{"name": %q, "state": %q}`, r.URL.Path, state)
					return
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(srv.Close)
			opts := []option.ClientOption{
				option.WithEndpoint(srv.URL + "/"),
				option.WithHTTPClient(srv.Client()),
			}

			res, err := VerifyRemote(context.Background(), testProject, testRegion,
				[]byte(verifyManifest), opts...)
			if err != nil {
				t.Fatalf("VerifyRemote() error = %v", err)
			}
			if len(res.Missing) != 1 || !strings.Contains(res.Missing[0], state) {
				t.Errorf("Missing = %v, want the %s version reported", res.Missing, state)
			}
		})
	}
}

// TestVerifyRemoteReadsTheVersionFromTheSecretPath checks that the version is looked up even when
// it is embedded in the name (projects/<p>/secrets/<s>/versions/<v>).
// secretResourceName handles this form explicitly, so if only the version extraction disagreed,
// it would end up "always looking at latest".
func TestVerifyRemoteReadsTheVersionFromTheSecretPath(t *testing.T) {
	manifest := strings.Replace(verifyManifest,
		"              name: api-token\n              key: latest",
		"              name: projects/other-project/secrets/api-token/versions/7", 1)
	recorded, opts := startVerifyAPI(t, func(string) int { return http.StatusOK })

	if _, err := VerifyRemote(context.Background(), testProject, testRegion, []byte(manifest), opts...); err != nil {
		t.Fatalf("VerifyRemote() error = %v", err)
	}
	got := pathsMatching(recorded(), "/versions/")
	if len(got) != 1 || !strings.Contains(got[0], "/secrets/api-token/versions/7") {
		t.Errorf("version lookups = %v, want the version from the secret path", got)
	}
}

// TestVerifyRemoteHandlesADomainScopedCloudSQLProject checks that a connection name containing a
// domain-scoped project (example.com:my-project) is handled.
// Splitting it into three from the left turns a valid value into a "wrong shape" warning every
// time.
func TestVerifyRemoteHandlesADomainScopedCloudSQLProject(t *testing.T) {
	manifest := strings.Replace(verifyRefsManifest,
		"cloudsql-instances: other-project:asia-northeast1:main-db",
		"cloudsql-instances: example.com:other-project:asia-northeast1:main-db", 1)
	recorded, opts := startVerifyAPI(t, func(string) int { return http.StatusOK })

	res, err := VerifyRemote(context.Background(), testProject, testRegion, []byte(manifest), opts...)
	if err != nil {
		t.Fatalf("VerifyRemote() error = %v", err)
	}
	if len(res.Unchecked) != 0 {
		t.Errorf("Unchecked = %v, want the domain-scoped project accepted", res.Unchecked)
	}
	got := pathsMatching(recorded(), "/instances/")
	if len(got) != 1 || !strings.Contains(got[0], "example.com:other-project") {
		t.Errorf("Cloud SQL lookups = %v, want the domain-scoped project kept whole", got)
	}
}
