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
func TestLoadRejectsMoreThanOneKind(t *testing.T) {
	for content, want := range map[string]string{
		"service: my-svc\njob: my-job\n":                      "sets job and service",
		"service: my-svc\nworkerPool: my-pool\n":              "sets service and workerPool",
		"job: my-job\nworkerPool: my-pool\n":                  "sets job and workerPool",
		"service: my-svc\njob: my-job\nworkerPool: my-pool\n": "sets job, service and workerPool",
	} {
		_, err := Load(write(t, content))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load(%q) error = %v, want it to contain %q", content, err, want)
		}
	}
	c, err := Load(write(t, "workerPool: my-pool\n"))
	if err != nil || c.WorkerPool != "my-pool" {
		t.Errorf("Load(workerPool) = %+v, %v", c, err)
	}
}
