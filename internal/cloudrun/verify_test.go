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
		"short_only": "shorthand", // 異常系: projects/ 接頭辞なし
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
							{Name: "C", ValueFrom: &run.EnvVarSource{SecretKeyRef: &run.SecretKeySelector{Name: "s1", Key: "1"}}}, // 重複
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

// --- VerifyRemote のリモート経路 ---

// verifyManifest は実行サービスアカウントとシークレットを参照するマニフェスト。
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

// startVerifyAPI は IAM と Secret Manager の両方に応えるフェイク API を立て、
// 受け取ったリクエストパスを記録する。両サービスとも同じ endpoint 指定を使うので、
// 1 つのサーバでパスによって振り分ける。
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

// TestVerifyRemoteLooksUpTheServiceAccountAcrossProjects は、実行サービスアカウントを
// プロジェクト非依存で引くことを確認する。Cloud Run は別プロジェクトの SA を実行 SA に
// できるので、検証対象のプロジェクトで固定すると正当な構成が 404 = Missing になる。
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

// TestVerifyRemoteTreatsOtherFailuresAsUnchecked は、権限不足などを Missing ではなく
// Unchecked に振り分けることを確認する。これを失敗にすると、ambient な project/region を
// 持つだけの CI のオフライン lint を壊してしまう。
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

// TestVerifyRemoteSkipsWhatTheManifestDoesNotReference は、参照が無ければ API を
// 叩かないことを確認する。
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
