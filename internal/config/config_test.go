package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clrnd.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoad(t *testing.T) {
	path := write(t, `project: my-project
region: asia-northeast1
tfstate:
  - location: gs://bucket/app/default.tfstate
  - name: network
    location: gs://bucket/net/default.tfstate
`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.Project != "my-project" || c.Region != "asia-northeast1" {
		t.Fatalf("got %+v", c)
	}
	if len(c.Tfstate) != 2 ||
		c.Tfstate[0].Location != "gs://bucket/app/default.tfstate" ||
		c.Tfstate[1].Name != "network" {
		t.Fatalf("tfstate = %+v", c.Tfstate)
	}
}

func TestLoadStrictRejectsUnknownKey(t *testing.T) {
	path := write(t, "proejct: typo\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Load() error = %v, want unknown field", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yml")); err == nil {
		t.Fatal("Load() = nil error, want error for missing file")
	}
}

func TestLoadJob(t *testing.T) {
	c, err := Load(write(t, "job: my-job\nmanifest: job.yaml\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.Job != "my-job" || c.Service != "" {
		t.Fatalf("Load() = %+v, want job my-job and no service", c)
	}
}

// TestLoadRejectsServiceAndJob checks that a config cannot describe a service and a job at once:
// it has one manifest, and a command of either kind would pick the wrong half up.
func TestLoadRejectsServiceAndJob(t *testing.T) {
	_, err := Load(write(t, "service: my-svc\njob: my-job\n"))
	if err == nil || !strings.Contains(err.Error(), "sets both service and job") {
		t.Fatalf("Load() error = %v, want it to refuse service and job together", err)
	}
}
