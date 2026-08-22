package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/masasuzu/clrnd/internal/config"
	"google.golang.org/api/option"
)

// liveServiceJSON はフェイク API が返す live サービス定義。
const liveServiceJSON = `{
  "apiVersion": "serving.knative.dev/v1",
  "kind": "Service",
  "metadata": {"name": "my-svc", "namespace": "test-project", "uid": "abc-123", "generation": 7},
  "spec": {"template": {"spec": {"containers": [{"image": "gcr.io/project/image:old"}]}}},
  "status": {"latestReadyRevisionName": "my-svc-00007-abc"}
}`

// localManifest は cmd に食わせるローカルのマニフェスト。live とはイメージタグだけ違う。
const localManifest = `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      - image: gcr.io/project/image:new
`

// startFakeAPI は Cloud Run Admin API の代わりに使う httptest サーバを立て、clientOptions を
// そこへ向ける。テスト終了時に元へ戻す。
func startFakeAPI(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	saved := clientOptions
	clientOptions = []option.ClientOption{
		option.WithEndpoint(srv.URL + "/"),
		option.WithHTTPClient(srv.Client()),
	}
	t.Cleanup(func() { clientOptions = saved })
}

// executeRoot はルートコマンドを引数付きで実行し、stdout/stderr を返す。
// rootCmd はパッケージ変数なので、テスト間で状態が漏れないよう cfg / configPath を退避する。
// (cobra はフラグ変数を巻き戻さないため、各テストは必要なフラグを毎回明示的に渡すこと。)
func executeRoot(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	savedCfg, savedDir, savedPath := cfg, configDir, configPath
	t.Cleanup(func() { cfg, configDir, configPath = savedCfg, savedDir, savedPath })
	cfg, configDir, configPath = &config.Config{}, "", ""

	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})

	err = rootCmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

// writeManifest は一時ディレクトリにマニフェストを書き、そのパスを返す。
func writeManifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write the manifest: %v", err)
	}
	return path
}

// TestDiffEndToEnd はフラグ解決 → クライアント生成 → API 取得 → 正規化 → diff 出力までを
// 一気通貫で確認する。
func TestDiffEndToEnd(t *testing.T) {
	var gotPath string
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceJSON))
	})

	manifest := writeManifest(t, localManifest)
	stdout, _, err := executeRoot(t, "diff", "my-svc", manifest,
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("diff error = %v", err)
	}

	wantPath := "/apis/serving.knative.dev/v1/namespaces/test-project/services/my-svc"
	if gotPath != wantPath {
		t.Errorf("requested path = %q, want %q", gotPath, wantPath)
	}
	if !strings.Contains(stdout, "-      - image: gcr.io/project/image:old") ||
		!strings.Contains(stdout, "+      - image: gcr.io/project/image:new") {
		t.Errorf("diff stdout = %q, want the image change", stdout)
	}
	// サーバ管理フィールドは正規化で落ちるので diff には出ない。
	if strings.Contains(stdout, "uid:") || strings.Contains(stdout, "status:") {
		t.Errorf("diff stdout leaks server-managed fields:\n%s", stdout)
	}
}

// TestDiffUsesConfigFile は config ファイルだけで project/region/service/manifest が
// 解決できることを確認する。
func TestDiffUsesConfigFile(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceJSON))
	})

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(localManifest), 0o600); err != nil {
		t.Fatalf("failed to write the manifest: %v", err)
	}
	configFile := filepath.Join(dir, "clrnd.yml")
	configYAML := "project: test-project\nregion: asia-northeast1\nservice: my-svc\nmanifest: manifest.yaml\n"
	if err := os.WriteFile(configFile, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("failed to write the config: %v", err)
	}

	// フラグ由来の値がテスト間で残らないよう、project/region は明示的に空へ戻す。
	stdout, _, err := executeRoot(t, "diff", "--config", configFile, "--project", "", "--region", "")
	if err != nil {
		t.Fatalf("diff error = %v", err)
	}
	if !strings.Contains(stdout, "+      - image: gcr.io/project/image:new") {
		t.Errorf("diff stdout = %q, want the image change", stdout)
	}
}

// TestDeployDryRunEndToEnd は deploy が差分を stdout に出し、dryRun=all を付けて
// ReplaceService を呼ぶことを確認する。
func TestDeployDryRunEndToEnd(t *testing.T) {
	var putQuery string
	var putBody []byte
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			putQuery = r.URL.RawQuery
			putBody, _ = io.ReadAll(r.Body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceJSON))
	})

	manifest := writeManifest(t, localManifest)
	stdout, _, err := executeRoot(t, "deploy", "my-svc", manifest,
		"--project", "test-project", "--region", "asia-northeast1", "--dry-run")
	if err != nil {
		t.Fatalf("deploy error = %v", err)
	}
	if !strings.Contains(stdout, "+      - image: gcr.io/project/image:new") {
		t.Errorf("deploy stdout = %q, want the diff", stdout)
	}
	if !strings.Contains(putQuery, "dryRun=all") {
		t.Errorf("ReplaceService query = %q, want dryRun=all", putQuery)
	}
	var sent map[string]interface{}
	if err := json.Unmarshal(putBody, &sent); err != nil {
		t.Fatalf("failed to parse the sent body: %v", err)
	}
	meta, _ := sent["metadata"].(map[string]interface{})
	if meta["namespace"] != "test-project" {
		t.Errorf("sent namespace = %v, want test-project", meta["namespace"])
	}
}

// TestVerifyLocalOnlyNeedsNoAPI は --local-only がクレデンシャルも API も要求しないことを
// 確認する (CI でのオフライン検証)。
func TestVerifyLocalOnlyNeedsNoAPI(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected API call to %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})

	manifest := writeManifest(t, localManifest)
	if _, _, err := executeRoot(t, "verify", "my-svc", manifest, "--local-only"); err != nil {
		t.Fatalf("verify --local-only error = %v", err)
	}
}

// TestVersion は --version がバージョンを出すことを確認する。
func TestVersion(t *testing.T) {
	stdout, _, err := executeRoot(t, "--version")
	if err != nil {
		t.Fatalf("--version error = %v", err)
	}
	if !strings.Contains(stdout, "clrnd version") {
		t.Errorf("--version stdout = %q, want it to name the binary and version", stdout)
	}
}
