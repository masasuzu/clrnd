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
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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

// liveServiceWithRevisionJSON は live のサービス定義。Cloud Run は取得時に必ず
// spec.template.metadata.name (サーバ採番のリビジョン名) を埋めて返す。
const liveServiceWithRevisionJSON = `{
  "apiVersion": "serving.knative.dev/v1",
  "kind": "Service",
  "metadata": {"name": "my-svc", "namespace": "test-project", "uid": "abc-123", "generation": 7},
  "spec": {"template": {
    "metadata": {"name": "my-svc-00007-abc"},
    "spec": {"containers": [{"image": "gcr.io/project/image:old"}]}
  }},
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

// resetFlags は cobra が巻き戻さないフラグ値を既定へ戻す。フラグは package 変数に
// bind されているため、これをしないと前のテストの値が次のテストへ漏れる。
func resetFlags(c *cobra.Command) {
	c.Flags().VisitAll(func(f *pflag.Flag) {
		// StringArray などは Set が追記になるので、スライス系は Replace で空にする。
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			_ = sv.Replace(nil)
		} else {
			_ = f.Value.Set(f.DefValue)
		}
		f.Changed = false
	})
	for _, sub := range c.Commands() {
		resetFlags(sub)
	}
}

// clearTargetEnv は gcloud 互換の環境変数を空にする。開発者や CI の環境に
// CLOUDSDK_CORE_PROJECT などが設定されていてもテストの結果が変わらないようにする。
func clearTargetEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{envProjectPrimary, envProjectSecondary, envRegionPrimary, envRegionSecondary} {
		t.Setenv(key, "")
	}
}

// executeRoot はルートコマンドを引数付きで実行し、stdout/stderr を返す。
// rootCmd はパッケージ変数なので、テスト間で状態が漏れないよう cfg / フラグ / 環境変数を
// 実行のたびに初期化する。
func executeRoot(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	clearTargetEnv(t)

	savedCfg, savedDir := cfg, configDir
	t.Cleanup(func() { cfg, configDir = savedCfg, savedDir })
	cfg, configDir = &config.Config{}, ""

	resetFlags(rootCmd)
	t.Cleanup(func() { resetFlags(rootCmd) })

	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		// nil に戻すと cobra が os.Args[1:] (= go test のフラグ) を読んでしまう。
		rootCmd.SetArgs([]string{})
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
	var gotPath string
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
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

	// 位置引数もフラグも渡さない: service / manifest / project / region がすべて
	// config から解決されることを確認する。
	stdout, _, err := executeRoot(t, "diff", "--config", configFile)
	if err != nil {
		t.Fatalf("diff error = %v", err)
	}
	// config の project がリクエストパスに反映されている (環境変数由来ではない)。
	wantPath := "/apis/serving.knative.dev/v1/namespaces/test-project/services/my-svc"
	if gotPath != wantPath {
		t.Errorf("requested path = %q, want %q", gotPath, wantPath)
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

// TestDeployValidatesManifestBeforeResolvingTarget は、マニフェストのローカル検証が
// project/region の解決やクライアント生成 (ADC 探索) より先に行われることを確認する。
// 順序が逆だと、認証情報や target が無い環境でマニフェストの問題が別のエラーに隠れる。
func TestDeployValidatesManifestBeforeResolvingTarget(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected API call to %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})

	// service 引数と metadata.name が食い違うマニフェスト。project/region はどこにも
	// 設定されていない (executeRoot が環境変数を空にし、config も無い)。
	manifest := writeManifest(t, localManifest)
	_, _, err := executeRoot(t, "deploy", "other-svc", manifest)
	if err == nil {
		t.Fatal("deploy error = nil, want a manifest validation error")
	}
	if !strings.Contains(err.Error(), "does not match service argument") {
		t.Errorf("deploy error = %v, want the manifest problem to surface first", err)
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

// TestInitScaffoldsWithoutRevisionName は init が live のリビジョン名を落として
// マニフェストを書き出すことを確認する。残すとテンプレートを変えた 2 回目の deploy が
// 「同名リビジョンは再作成できない」で失敗する。
func TestInitScaffoldsWithoutRevisionName(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceWithRevisionJSON))
	})

	dir := t.TempDir()
	t.Chdir(dir)

	if _, _, err := executeRoot(t, "init", "my-svc",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("init error = %v", err)
	}

	manifest, err := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	if err != nil {
		t.Fatalf("failed to read the scaffolded manifest: %v", err)
	}
	if strings.Contains(string(manifest), "my-svc-00007-abc") {
		t.Errorf("scaffolded manifest pins the revision name:\n%s", manifest)
	}
	// 空になった spec.template.metadata ごと消えていること。
	if strings.Contains(string(manifest), "metadata: {}") {
		t.Errorf("scaffolded manifest keeps an empty template metadata:\n%s", manifest)
	}
}

// TestDiffIsEmptyRightAfterInit は init 直後の diff が空であることを確認する。
// live は必ずリビジョン名を持ち、init はそれを落とすので、比較時に live 側のリビジョン名を
// 無視しないと「消えない差分」が出続ける。
func TestDiffIsEmptyRightAfterInit(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceWithRevisionJSON))
	})

	dir := t.TempDir()
	t.Chdir(dir)

	if _, _, err := executeRoot(t, "init", "my-svc",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("init error = %v", err)
	}

	// init が書いた clrnd.yml から service/manifest/project/region を解決させる。
	stdout, _, err := executeRoot(t, "diff")
	if err != nil {
		t.Fatalf("diff error = %v", err)
	}
	if stdout != "" {
		t.Errorf("diff right after init = %q, want empty", stdout)
	}
}

// TestVerifyWarnsWhenRevisionNameIsPinned は、リビジョン名を固定したマニフェストに対して
// verify が警告を出しつつ成功することを確認する (使い捨てのデプロイでは正しい書き方なので
// 失敗にはしない)。
func TestVerifyWarnsWhenRevisionNameIsPinned(t *testing.T) {
	manifest := writeManifest(t, `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    metadata:
      name: my-svc-00007-abc
    spec:
      containers:
      - image: gcr.io/project/image:new
`)

	_, stderr, err := executeRoot(t, "verify", "my-svc", manifest, "--local-only")
	if err != nil {
		t.Fatalf("verify error = %v, want success with a warning", err)
	}
	if !strings.Contains(stderr, "warning:") || !strings.Contains(stderr, "my-svc-00007-abc") {
		t.Errorf("verify stderr = %q, want a warning naming the pinned revision", stderr)
	}
}

// TestDeployWarnsWhenRevisionNameIsPinned は deploy でも同じ警告が出ることを確認する。
// deploy だけを回す CI では verify の警告を見る機会が無く、Cloud Run からの 409 だけが
// 出て原因が分からないため。
func TestDeployWarnsWhenRevisionNameIsPinned(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceWithRevisionJSON))
	})

	manifest := writeManifest(t, `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    metadata:
      name: my-svc-00007-abc
    spec:
      containers:
      - image: gcr.io/project/image:new
`)

	_, stderr, err := executeRoot(t, "deploy", "my-svc", manifest,
		"--project", "test-project", "--region", "asia-northeast1", "--dry-run")
	if err != nil {
		t.Fatalf("deploy error = %v", err)
	}
	if !strings.Contains(stderr, "warning:") || !strings.Contains(stderr, "my-svc-00007-abc") {
		t.Errorf("deploy stderr = %q, want a warning naming the pinned revision", stderr)
	}
}

// TestDeployDoesNotWarnWithoutRevisionName は通常のマニフェストで警告が出ないことを確認する。
func TestDeployDoesNotWarnWithoutRevisionName(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceJSON))
	})

	manifest := writeManifest(t, localManifest)
	_, stderr, err := executeRoot(t, "deploy", "my-svc", manifest,
		"--project", "test-project", "--region", "asia-northeast1", "--dry-run")
	if err != nil {
		t.Fatalf("deploy error = %v", err)
	}
	if strings.Contains(stderr, "warning:") {
		t.Errorf("deploy stderr = %q, want no warning", stderr)
	}
}

// TestVerifyDoesNotWarnWithoutRevisionName は、リビジョン名を書いていない通常のマニフェスト
// では警告が出ないことを確認する。
func TestVerifyDoesNotWarnWithoutRevisionName(t *testing.T) {
	manifest := writeManifest(t, localManifest)

	_, stderr, err := executeRoot(t, "verify", "my-svc", manifest, "--local-only")
	if err != nil {
		t.Fatalf("verify error = %v", err)
	}
	if strings.Contains(stderr, "warning:") {
		t.Errorf("verify stderr = %q, want no warning", stderr)
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
