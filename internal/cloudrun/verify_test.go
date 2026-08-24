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

// arManifest は Artifact Registry のイメージ (タグ指定と入れ子パスのダイジェスト指定) を
// 参照するマニフェスト。SA もシークレットも持たないので、確認されるのはイメージだけ。
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

// TestVerifyRemoteChecksArtifactRegistryImages は、イメージの実在確認が
// タグ指定とダイジェスト指定でそれぞれ正しいリソースを引くことを確認する。
// ロケーションもプロジェクトもイメージ参照から取るので、別プロジェクトのイメージが
// そのまま通る (実行 SA と同じ扱い)。
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

// TestVerifyRemoteReportsAMissingImage は、404 だけを Missing にすることを確認する。
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

// TestVerifyRemoteTreatsAnInaccessibleImageAsUnchecked は、403 を Missing にしないことを
// 確認する。実 API では存在しない (またはアクセスできない) プロジェクトが 403 を返すので、
// ここを Missing にすると正当な構成の verify を落とす。
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

// TestVerifyRemoteSkipsRegistriesItCannotCheck は、確認できないレジストリについては
// 何も言わないことを確認する。ここを警告にすると、Docker Hub のイメージを使っている
// だけで毎回 warning が出て、警告そのものが読み飛ばされるようになる。
func TestVerifyRemoteSkipsRegistriesItCannotCheck(t *testing.T) {
	recorded, opts := startVerifyAPI(t, func(string) int { return http.StatusOK })

	// validManifest のイメージは gcr.io。
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

// --- VPC コネクタ / Cloud SQL / シークレットのバージョン ---

// verifyRefsManifest は VPC コネクタ (短縮名) と Cloud SQL を参照するマニフェスト。
// シークレットは版を明示している。
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

// pathsMatching は記録されたリクエストのうち、部分文字列を含むものを返す。
func pathsMatching(paths []string, substr string) []string {
	var out []string
	for _, p := range paths {
		if strings.Contains(p, substr) {
			out = append(out, p)
		}
	}
	return out
}

// TestVerifyRemoteChecksTheVPCConnector は、短縮名のコネクタがデプロイ先の
// プロジェクトとリージョンで完全なリソース名に補われることを確認する。コネクタは
// リージョナルなリソースなので、ここだけリージョンが要る。
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

// TestVerifyRemoteKeepsAFullyQualifiedConnector は、完全なリソース名で書かれている
// 場合にそれをそのまま使うことを確認する (別プロジェクト・別リージョンのコネクタを
// デプロイ先の値で上書きしない)。
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

// TestVerifyRemoteReportsMissingReferences は、404 が返る参照が Missing になることを
// 確認する。どれもデプロイして初めて落ちる種類の参照。
func TestVerifyRemoteReportsMissingReferences(t *testing.T) {
	recorded, opts := startVerifyAPI(t, func(path string) int {
		// シークレット本体は在るが、指定された版だけが無い状況を作る。
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
	// Cloud SQL は接続名のプロジェクトで引く (デプロイ先で固定しない)。
	if got := pathsMatching(recorded(), "/instances/"); len(got) != 1 ||
		!strings.Contains(got[0], "other-project") {
		t.Errorf("Cloud SQL lookups = %v, want the project from the connection name", got)
	}
}

// TestVerifyRemoteSkipsVersionsOfAMissingSecret は、シークレット自体が無い場合に
// その版を問い合わせないことを確認する。両方並べても分かることは増えず、本当の原因が
// 埋もれる。
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

// TestVerifyRemoteReportsAMalformedCloudSQLConnection は、接続名の形が違うものを
// 「無い」ではなく「確かめられない」に倒すことを確認する。
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

// TestVerifyRemoteSkipsUnreferencedAPIs は、アノテーションが無いマニフェストで
// VPC / Cloud SQL の API を触らないことを確認する。使っていない API の有効化を
// verify のために要求しない。
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
