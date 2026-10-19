//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// These tests run offline. CI explicitly selects TestHarness and never TestE2E.
func TestHarnessEnvironment(t *testing.T) {
	original := []string{"A=old", "B=kept"}
	got := environment(original, map[string]string{"A": "new", "C": "added"})
	if strings.Join(original, ",") != "A=old,B=kept" {
		t.Fatal("environment mutated its input")
	}
	for _, want := range []string{"A=new", "B=kept", "C=added"} {
		if !strings.Contains(strings.Join(got, "\n"), want) {
			t.Fatalf("missing %s", want)
		}
	}
	if strings.Contains(strings.Join(got, "\n"), "A=old") {
		t.Fatal("duplicate override")
	}
}
func TestHarnessSettings(t *testing.T) {
	here := t.TempDir()
	caller := t.TempDir()
	for _, key := range []string{"PROJECT", "REPO", "ONLY", "OLD_REF", "WORK_ROOT", "CLRND_E2E_GCLOUD_CONFIG", "GOOGLE_APPLICATION_CREDENTIALS", "CLRND_E2E_CLEANUP_ORPHANS"} {
		t.Setenv(key, "")
	}
	if _, err := readSettingsIn(here, caller); err == nil || !strings.Contains(err.Error(), "no project") {
		t.Fatalf("missing project error = %v", err)
	}
	t.Setenv("PROJECT", "test-project")
	t.Setenv("WORK_ROOT", "scratch")
	adcDir := filepath.Join(caller, "credentials")
	if err := os.Mkdir(adcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	adc := filepath.Join(adcDir, "application_default_credentials.json")
	if err := os.WriteFile(adc, []byte(`{"type":"impersonated_service_account"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLRND_E2E_GCLOUD_CONFIG", "credentials")
	c, err := readSettingsIn(here, caller)
	if err != nil {
		t.Fatal(err)
	}
	if c.workRoot != filepath.Join(caller, "scratch") {
		t.Fatalf("work root = %s", c.workRoot)
	}
	for _, want := range []string{"CLOUDSDK_CONFIG=" + adcDir, "GOOGLE_APPLICATION_CREDENTIALS=" + adc} {
		if !strings.Contains(strings.Join(c.env, "\n"), want) {
			t.Fatalf("missing %s", want)
		}
	}
	t.Setenv("CLRND_E2E_GCLOUD_CONFIG", "")
	relative, err := filepath.Rel(here, adcDir)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(here, "gcloud-config.env"), []byte(relative+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = readSettingsIn(here, caller)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(c.env, "\n"), "GOOGLE_APPLICATION_CREDENTIALS="+adc) {
		t.Fatal("file path was not resolved against its directory")
	}
	if err = os.WriteFile(adc, []byte(`{"type":"authorized_user"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = readSettingsIn(here, caller); err == nil || !strings.Contains(err.Error(), "not a service account") {
		t.Fatalf("user ADC error = %v", err)
	}
	t.Setenv("ONLY", "bogus")
	if _, err = readSettingsIn(here, caller); err == nil || !strings.Contains(err.Error(), "unknown ONLY") {
		t.Fatalf("ONLY error = %v", err)
	}
}
func TestHarnessRedaction(t *testing.T) {
	text := "project-secret clrnd-e2e-20261019123456-abc-job-x7 https://svc-123456789012.run.app ci@example.com 123456789012 tester@project-secret.iam.gserviceaccount.com"
	got := redact(text, "project-secret")
	for _, secret := range []string{"project-secret", "clrnd-e2e-", "run.app", "ci@example.com", "123456789012", "tester@", "gserviceaccount.com"} {
		if strings.Contains(got, secret) {
			t.Fatalf("identifier remains: %s", got)
		}
	}
}
func TestHarnessManifests(t *testing.T) {
	for _, b := range [][]byte{serviceManifest("svc", "img"), jobManifest("job", "img")} {
		var m map[string]any
		if err := yaml.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		setEnv(m, "first")
		setEnv(m, "second")
		container := task(m)["containers"].([]any)[0].(map[string]any)
		env := container["env"].([]any)
		if len(env) != 1 || env[0].(map[string]any)["value"] != "second" {
			t.Fatalf("env = %#v", env)
		}
		if container["image"] != "img" {
			t.Fatal("lost the image")
		}
	}
}
func TestHarnessCommand(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := &suite{ctx: t.Context(), c: settings{project: "test-project", env: os.Environ()}}
	r := s.command(t, t.Context(), t.TempDir(), map[string]string{"CLRND_E2E_HELPER": "output"}, exe, "-test.run=^TestHarnessChild$")
	if r.code != 7 || r.stdout != "stdout-data\n" || r.stderr != "stderr-progress\n" {
		t.Fatalf("result = %+v", r)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	r = s.command(t, ctx, t.TempDir(), map[string]string{"CLRND_E2E_HELPER": "sleep"}, exe, "-test.run=^TestHarnessChild$")
	if r.code == 0 || ctx.Err() == nil {
		t.Fatal("child did not stop on cancellation")
	}
}

// The cloud test must skip without the guard. PATH is emptied so that, if the guard were gone, the
// child fails on its tool lookup instead of reaching any project.
func TestHarnessGuard(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := &suite{ctx: t.Context(), c: settings{project: "harness-guard", env: os.Environ()}}
	r := s.command(t, t.Context(), t.TempDir(), map[string]string{cloudGuard: "", "PATH": "", "PROJECT": "harness-guard"},
		exe, "-test.run=^TestE2E$", "-test.v")
	if r.code != 0 || !strings.Contains(r.stdout, "--- SKIP: TestE2E") || !strings.Contains(r.stdout, cloudGuard) {
		t.Fatalf("TestE2E ran without %s: %+v", cloudGuard, r)
	}
}
func TestHarnessParallelTranscript(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "commands.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &suite{ctx: t.Context(), c: settings{project: "test-project", env: os.Environ()}, transcript: f}
	const runs = 4
	t.Run("group", func(t *testing.T) {
		for i := range runs {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()
				s.command(t, t.Context(), t.TempDir(), map[string]string{"CLRND_E2E_HELPER": "output"}, exe, "-test.run=^TestHarnessChild$")
			})
		}
	})
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range runs {
		entry := fmt.Sprintf("[%s/group/%d]\n$ ", t.Name(), i)
		if !strings.Contains(string(b), entry) {
			t.Fatalf("transcript lacks an intact entry for %d:\n%s", i, b)
		}
	}
	if n := strings.Count(string(b), "stdout:\nstdout-data\nstderr:\nstderr-progress\n"); n != runs {
		t.Fatalf("transcript has %d intact outputs, want %d:\n%s", n, runs, b)
	}
}
func TestHarnessChild(t *testing.T) {
	switch os.Getenv("CLRND_E2E_HELPER") {
	case "output":
		b := make([]byte, 1)
		if n, _ := os.Stdin.Read(b); n != 0 {
			os.Exit(8)
		}
		fmt.Println("stdout-data")
		fmt.Fprintln(os.Stderr, "stderr-progress")
		os.Exit(7)
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}
func TestHarnessNotFound(t *testing.T) {
	for _, tc := range []struct {
		r    result
		want bool
	}{
		{result{code: 1, stderr: "ERROR: NOT_FOUND"}, true},
		{result{code: 1, stderr: "ERROR: (gcloud.run.jobs.describe) Cannot find job [missing]."}, true},
		{result{code: 1, stderr: "ERROR: (gcloud.run.services.describe) Cannot find service [missing]."}, true},
		{result{code: 1, stderr: "ERROR: (gcloud.run.revisions.describe) Cannot find revision [missing]"}, true},
		{result{code: 1, stderr: "ERROR: (gcloud.run.jobs.delete) Job [missing] could not be found."}, true},
		{result{code: 1, stderr: "ERROR: Cannot find credentials"}, false},
		{result{code: 1, stderr: "PERMISSION_DENIED"}, false},
		{result{code: 0, stdout: "not found"}, false},
		{result{code: -1, stderr: `exec: "gcloud": executable file not found in $PATH`}, false},
		{result{code: -1, stderr: "Error 404\nsignal: killed"}, false},
	} {
		if got := notFound(tc.r); got != tc.want {
			t.Errorf("notFound(%+v)=%v", tc.r, got)
		}
	}
}
func TestHarnessYAMLRetainsValues(t *testing.T) {
	m := map[string]any{"kind": "Service", "metadata": map[string]any{"name": "svc"}}
	pinRevision(m, "svc-pinned")
	if !reflect.DeepEqual(mapping(m, "spec", "template", "metadata"), map[string]any{"name": "svc-pinned"}) {
		t.Fatal("revision name not set")
	}
}

func TestHarnessGitArchive(t *testing.T) {
	var data bytes.Buffer
	w := tar.NewWriter(&data)
	// git archive starts with a global PAX header carrying the commit hash.
	if err := w.WriteHeader(&tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "commit-hash"}}); err != nil {
		t.Fatal(err)
	}
	content := "module example.com/old\n"
	if err := w.WriteHeader(&tar.Header{Name: "go.mod", Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "old.tar")
	if err := os.WriteFile(archive, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := extractArchive(archive, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "go.mod"))
	if err != nil || string(got) != content {
		t.Fatalf("extracted content = %q, error = %v", got, err)
	}
}
