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

// startInitAPI starts a fake API that returns the live service init reads.
func startInitAPI(t *testing.T) {
	t.Helper()
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveServiceStatusJSON))
	})
}

// TestInitWritesTheConfigWhereItWasAskedTo checks that the config is written to the location given
// with -c. If the place it reads from and the place it writes to disagree, passing
// -c infra/clrnd.yml still produces ./clrnd.yml.
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

// TestInitRecordsTheManifestPathRelativeToTheConfig checks that the recorded manifest path is
// relative to the config file. resolveConfigPath resolves it against the config's directory, so
// recording it relative to the cwd breaks the path.
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
	// The manifest is written to the cwd, so seen from infra/ it is one level up.
	if cfg.Manifest != filepath.Join("..", "manifest.yaml") {
		t.Errorf("manifest = %q, want it relative to the config directory", cfg.Manifest)
	}
}

// Use a path under a directory that does not exist as the config destination. init "reads
// --config only when it already exists", so loadConfig lets it through and only the config write
// *after* the manifest has been written fails. Making the config itself a directory would also be
// an option, but then loadConfig tries to read it as a config file and fails, ending before
// runInit is entered (the test would think it was verifying the restore while only looking at a
// state in which nothing was ever overwritten).
const unwritableConfig = "nodir/clrnd.yml"

// TestInitRestoresTheManifestWhenTheConfigWriteFails checks that when writing the config fails,
// the manifest overwritten by --force is put back. Without that, a hand-edited manifest is left
// overwritten with the live contents, and there is no config either.
func TestInitRestoresTheManifestWhenTheConfigWriteFails(t *testing.T) {
	startInitAPI(t)
	dir := t.TempDir()
	t.Chdir(dir)

	const original = "# hand-edited manifest\n"
	if err := os.WriteFile("manifest.yaml", []byte(original), 0o600); err != nil {
		t.Fatalf("failed to seed the manifest: %v", err)
	}

	_, _, err := executeRoot(t, "init", "my-svc", "--config", unwritableConfig, "--force",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil {
		t.Fatal("init error = nil, want the config write to fail")
	}
	// Make sure it failed on the config write that comes after writing the manifest. If it failed
	// earlier, the test would be looking at a state where "nothing was overwritten in the first
	// place" rather than at a restore.
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

// TestInitRemovesTheManifestItCreatedWhenTheConfigWriteFails checks that when there was no
// manifest to begin with and writing the config fails, the half-made manifest is not left behind.
// This is the other branch of restoreManifest.
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

// TestInitWritesRestrictivePermissions checks that the generated files cannot be read by other
// users. The live definition can contain plaintext environment variables.
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

// TestRenderRefusesToOverwriteItsInput checks that it refuses when -o is given the same file as
// the input. Letting it through would overwrite the render source with its own result.
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

// TestRenderWritesRestrictivePermissions checks that the rendered output cannot be read by other
// users. It can contain secrets via must_env and the like.
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

// TestInitReadsTheConfigWhenItAlreadyExists checks that when the config -c points to already
// exists, init reads it as well. If it were skipped to allow it as a write destination, the
// service/project written in the config would have no effect when regenerating with --force.
func TestInitReadsTheConfigWhenItAlreadyExists(t *testing.T) {
	startInitAPI(t)
	dir := t.TempDir()
	t.Chdir(dir)

	const existing = "project: test-project\nregion: asia-northeast1\nservice: my-svc\n"
	if err := os.WriteFile("clrnd.yml", []byte(existing), 0o600); err != nil {
		t.Fatalf("failed to seed the config: %v", err)
	}

	// Pass neither the service nor --project/--region. It fails unless the config fills them in.
	if _, _, err := executeRoot(t, "init", "--config", "clrnd.yml", "--force"); err != nil {
		t.Fatalf("init error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.yaml")); err != nil {
		t.Errorf("manifest.yaml was not written: %v", err)
	}
}

// TestMissingConfigStillFailsForOtherCommands checks that for commands other than init, a
// missing explicit --config is still an error as before. If the exception added for init spread
// to every command, a typo in the path would be silently ignored.
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

// TestRenderTightensThePermissionsOfAnExistingOutput checks that writing to an output that
// already exists still leaves it at 0600. os.WriteFile's perm only applies when the file is
// created, so writing rendered output containing secrets into a 0644 file used to leave it
// readable by anyone.
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

// TestInitTightensThePermissionsOfExistingFiles checks that overwriting existing files with
// --force also leaves them at 0600. The live definition can contain plaintext environment
// variables.
func TestInitTightensThePermissionsOfExistingFiles(t *testing.T) {
	startInitAPI(t)
	dir := t.TempDir()
	t.Chdir(dir)
	// clrnd.yml is read by auto-detection, so give it contents that parse.
	seed := map[string]string{
		"manifest.yaml": "# hand-edited manifest\n",
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

// TestWriteFilePrivateKeepsTheOldContentOnFailure checks that the existing content survives a
// failed write. Truncating before writing means that a failure partway through also loses the
// previous good content.
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
	// Make the write fail by preventing the temporary file from being created.
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

// TestWriteFilePrivateLeavesNoTemporaryFile checks that no temporary file is left behind when the
// rename fails. Otherwise junk with readable contents piles up next to the destination.
func TestWriteFilePrivateLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	// Make the rename fail by making the destination a directory.
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

// TestWriteFileExclusiveRefusesAnExistingFile checks that the path without --force does not
// overwrite an existing file. When the existence check and the write are separate operations, a
// file created in between is silently overwritten.
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

// TestWriteFileExclusiveCreatesAPrivateFile checks that a newly created file is 0600.
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
