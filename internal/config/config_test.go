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
