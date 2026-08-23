package cmd

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/masasuzu/clrnd/internal/config"
)

// startInitAPI は init が読む live サービスを返すフェイク API を立てる。
func startInitAPI(t *testing.T) {
	t.Helper()
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceStatusJSON))
	})
}

// TestInitWritesTheConfigWhereItWasAskedTo は、-c で指定した場所に config を書くことを
// 確認する。読む場所と書く場所が食い違うと、-c infra/clrnd.yml を渡したのに
// ./clrnd.yml が生まれる。
func TestInitWritesTheConfigWhereItWasAskedTo(t *testing.T) {
	startInitAPI(t)
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Mkdir("infra", 0o755); err != nil {
		t.Fatalf("failed to create the directory: %v", err)
	}

	if _, _, err := executeRoot(t, "init", "my-svc", "--config", "infra/clrnd.yml",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("init error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "infra", "clrnd.yml")); err != nil {
		t.Errorf("infra/clrnd.yml was not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "clrnd.yml")); err == nil {
		t.Error("./clrnd.yml was written even though --config pointed elsewhere")
	}
}

// TestInitRecordsTheManifestPathRelativeToTheConfig は、記録するマニフェストのパスが
// config ファイル基準になっていることを確認する。resolveConfigPath は config の
// ディレクトリ基準で解決するので、cwd 基準のまま記録するとパスが壊れる。
func TestInitRecordsTheManifestPathRelativeToTheConfig(t *testing.T) {
	startInitAPI(t)
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Mkdir("infra", 0o755); err != nil {
		t.Fatalf("failed to create the directory: %v", err)
	}

	if _, _, err := executeRoot(t, "init", "my-svc", "--config", "infra/clrnd.yml",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("init error = %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "infra", "clrnd.yml"))
	if err != nil {
		t.Fatalf("failed to read the config: %v", err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("failed to parse the config: %v", err)
	}
	// マニフェストは cwd に書かれるので、infra/ から見ると 1 つ上。
	if cfg.Manifest != filepath.Join("..", "manifest.yaml") {
		t.Errorf("manifest = %q, want it relative to the config directory", cfg.Manifest)
	}
}

// 存在しないディレクトリの下を config の書き込み先にする。init は「既に在る --config
// だけを読む」ので loadConfig は素通りし、マニフェストを書いた *後* の config 書き込み
// だけが失敗する。config 自体をディレクトリにする手もあるが、それだと loadConfig が
// それを設定ファイルとして読もうとして落ち、runInit に入る前に終わってしまう
// (復元を検証しているつもりで、書き換えが起きていないだけの状態を見ることになる)。
const unwritableConfig = "nodir/clrnd.yml"

// TestInitRestoresTheManifestWhenTheConfigWriteFails は、config の書き込みに失敗した
// ときに --force で潰したマニフェストが戻ることを確認する。戻さないと、手で編集した
// マニフェストが live の内容で潰れたまま config も無い状態が残る。
func TestInitRestoresTheManifestWhenTheConfigWriteFails(t *testing.T) {
	startInitAPI(t)
	dir := t.TempDir()
	t.Chdir(dir)

	const original = "# 手で編集したマニフェスト\n"
	if err := os.WriteFile("manifest.yaml", []byte(original), 0o600); err != nil {
		t.Fatalf("failed to seed the manifest: %v", err)
	}

	_, _, err := executeRoot(t, "init", "my-svc", "--config", unwritableConfig, "--force",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil {
		t.Fatal("init error = nil, want the config write to fail")
	}
	// マニフェストを書いた後の config 書き込みで落ちたことを確かめる。ここが手前で
	// 落ちていると、復元ではなく「そもそも書き換えていない」状態を見てしまう。
	if !strings.Contains(err.Error(), unwritableConfig) {
		t.Fatalf("init error = %v, want it to fail on writing the config", err)
	}

	got, readErr := os.ReadFile("manifest.yaml")
	if readErr != nil {
		t.Fatalf("the manifest is gone: %v", readErr)
	}
	if string(got) != original {
		t.Errorf("manifest = %q, want the original contents restored", got)
	}
}

// TestInitRemovesTheManifestItCreatedWhenTheConfigWriteFails は、元々マニフェストが
// 無かった場合に、config の書き込みに失敗したら作りかけのマニフェストを残さないことを
// 確認する。restoreManifest のもう一方の分岐。
func TestInitRemovesTheManifestItCreatedWhenTheConfigWriteFails(t *testing.T) {
	startInitAPI(t)
	dir := t.TempDir()
	t.Chdir(dir)

	_, _, err := executeRoot(t, "init", "my-svc", "--config", unwritableConfig,
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil {
		t.Fatal("init error = nil, want the config write to fail")
	}
	if !strings.Contains(err.Error(), unwritableConfig) {
		t.Fatalf("init error = %v, want it to fail on writing the config", err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, "manifest.yaml")); !os.IsNotExist(statErr) {
		t.Errorf("manifest.yaml is still there (stat error = %v), want the half-done scaffold removed", statErr)
	}
}

// TestInitWritesRestrictivePermissions は、生成物が他ユーザから読めないことを
// 確認する。live の定義には平文の環境変数が入りうる。
func TestInitWritesRestrictivePermissions(t *testing.T) {
	startInitAPI(t)
	dir := t.TempDir()
	t.Chdir(dir)

	if _, _, err := executeRoot(t, "init", "my-svc",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("init error = %v", err)
	}
	for _, name := range []string{"manifest.yaml", "clrnd.yml"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, perm)
		}
	}
}

// TestRenderRefusesToOverwriteItsInput は、-o に入力と同じファイルを渡したときに
// 断ることを確認する。通せばレンダリング元が結果で潰れる。
func TestRenderRefusesToOverwriteItsInput(t *testing.T) {
	manifest := writeManifest(t, localManifest)

	_, _, err := executeRoot(t, "render", manifest, "-o", manifest)
	if err == nil {
		t.Fatal("render error = nil, want it to refuse writing over its input")
	}
	if !strings.Contains(err.Error(), "refusing to write over the manifest") {
		t.Errorf("render error = %v", err)
	}
	got, readErr := os.ReadFile(manifest)
	if readErr != nil {
		t.Fatalf("failed to read the manifest: %v", readErr)
	}
	if string(got) != localManifest {
		t.Errorf("the input manifest was modified:\n%s", got)
	}
}

// TestRenderWritesRestrictivePermissions は、展開後の出力が他ユーザから読めない
// ことを確認する。must_env などで秘密を含みうる。
func TestRenderWritesRestrictivePermissions(t *testing.T) {
	manifest := writeManifest(t, localManifest)
	out := filepath.Join(t.TempDir(), "rendered.yaml")

	if _, _, err := executeRoot(t, "render", manifest, "-o", out); err != nil {
		t.Fatalf("render error = %v", err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("failed to stat the output: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

// TestInitReadsTheConfigWhenItAlreadyExists は、-c の指す config が既にある場合は
// init もそれを読むことを確認する。書き込み先として許すために読み飛ばしてしまうと、
// config に書いた service/project が --force での再生成時に効かなくなる。
func TestInitReadsTheConfigWhenItAlreadyExists(t *testing.T) {
	startInitAPI(t)
	dir := t.TempDir()
	t.Chdir(dir)

	const existing = "project: test-project\nregion: asia-northeast1\nservice: my-svc\n"
	if err := os.WriteFile("clrnd.yml", []byte(existing), 0o600); err != nil {
		t.Fatalf("failed to seed the config: %v", err)
	}

	// service も --project/--region も渡さない。config から埋まらなければ失敗する。
	if _, _, err := executeRoot(t, "init", "--config", "clrnd.yml", "--force"); err != nil {
		t.Fatalf("init error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.yaml")); err != nil {
		t.Errorf("manifest.yaml was not written: %v", err)
	}
}

// TestMissingConfigStillFailsForOtherCommands は、init 以外では明示した --config が
// 無いことが従来どおりエラーであることを確認する。init のために入れた例外が全コマンドへ
// 波及すると、パスの打ち間違いが黙って無視される。
func TestMissingConfigStillFailsForOtherCommands(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	_, _, err := executeRoot(t, "render", "--config", "nope.yml")
	if err == nil {
		t.Fatal("render error = nil, want an error for a missing --config")
	}
	if !strings.Contains(err.Error(), "nope.yml") {
		t.Errorf("render error = %v, want it to name the missing config", err)
	}
}

// TestRenderTightensThePermissionsOfAnExistingOutput は、既にある出力先へ書いても
// 0600 になることを確認する。os.WriteFile の perm は新規作成時にしか効かないので、
// 0644 のファイルへ秘密混じりの展開結果を書くと誰からも読める状態が残っていた。
func TestRenderTightensThePermissionsOfAnExistingOutput(t *testing.T) {
	manifest := writeManifest(t, localManifest)
	out := filepath.Join(t.TempDir(), "rendered.yaml")
	if err := os.WriteFile(out, []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("failed to seed the output: %v", err)
	}

	if _, _, err := executeRoot(t, "render", manifest, "-o", out); err != nil {
		t.Fatalf("render error = %v", err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("failed to stat the output: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("failed to read the output: %v", err)
	}
	if string(got) == "stale\n" {
		t.Error("the output was not replaced")
	}
}

// TestInitTightensThePermissionsOfExistingFiles は、--force で既存ファイルを潰す場合も
// 0600 になることを確認する。live の定義には平文の環境変数が入りうる。
func TestInitTightensThePermissionsOfExistingFiles(t *testing.T) {
	startInitAPI(t)
	dir := t.TempDir()
	t.Chdir(dir)
	// clrnd.yml は自動検出で読まれるので、パースできる中身にしておく。
	seed := map[string]string{
		"manifest.yaml": "# 手で編集したマニフェスト\n",
		"clrnd.yml":     "project: test-project\nregion: asia-northeast1\nservice: my-svc\n",
	}
	for name, content := range seed {
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatalf("failed to seed %s: %v", name, err)
		}
	}

	if _, _, err := executeRoot(t, "init", "my-svc", "--force",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("init error = %v", err)
	}
	for _, name := range []string{"manifest.yaml", "clrnd.yml"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, perm)
		}
	}
}

// TestWriteFilePrivateKeepsTheOldContentOnFailure は、書き込みに失敗しても既存の
// 内容が残ることを確認する。truncate してから書くと、途中で失敗した時点で以前の
// 正常な内容まで失われる。
func TestWriteFilePrivateKeepsTheOldContentOnFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "out.yaml")
	const original = "original\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("failed to seed the file: %v", err)
	}
	// 一時ファイルを作れないようにして書き込みを失敗させる。
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("failed to change the directory mode: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := writeFilePrivate(path, []byte("new\n")); err == nil {
		t.Fatal("writeFilePrivate() error = nil, want the write to fail")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the file is gone: %v", err)
	}
	if string(got) != original {
		t.Errorf("file = %q, want the original contents untouched", got)
	}
}

// TestWriteFilePrivateLeavesNoTemporaryFile は、rename に失敗しても一時ファイルを
// 残さないことを確認する。残ると出力先の隣に中身の見えるゴミが溜まる。
func TestWriteFilePrivateLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	// 出力先をディレクトリにして rename を失敗させる。
	path := filepath.Join(dir, "taken")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("failed to create the directory: %v", err)
	}

	if err := writeFilePrivate(path, []byte("new\n")); err == nil {
		t.Fatal("writeFilePrivate() error = nil, want the rename to fail")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("failed to read the directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "taken" {
		t.Errorf("directory holds %d entries, want only the original one", len(entries))
	}
}

// TestWriteFileExclusiveRefusesAnExistingFile は、--force が無い経路が既存ファイルを
// 上書きしないことを確認する。存在確認と書き込みが別操作だと、その隙に作られた
// ファイルを黙って潰す。
func TestWriteFileExclusiveRefusesAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clrnd.yml")
	const original = "original\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("failed to seed the file: %v", err)
	}

	err := writeFileExclusive(path, []byte("new\n"))
	if err == nil {
		t.Fatal("writeFileExclusive() error = nil, want it to refuse an existing file")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("writeFileExclusive() error = %v, want it to say the file exists", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("failed to read the file: %v", readErr)
	}
	if string(got) != original {
		t.Errorf("file = %q, want the original contents untouched", got)
	}
}

// TestWriteFileExclusiveCreatesAPrivateFile は、新規作成が 0600 になることを確認する。
func TestWriteFileExclusiveCreatesAPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clrnd.yml")

	if err := writeFileExclusive(path, []byte("new\n")); err != nil {
		t.Fatalf("writeFileExclusive() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat the file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}
