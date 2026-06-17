package render

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Terraform state v4 のミニマルなフィクスチャ。output と resource 属性を含む。
const tfstateFixture = `{
  "version": 4,
  "terraform_version": "1.7.0",
  "outputs": {
    "service_account": { "value": "run-sa@example.iam.gserviceaccount.com", "type": "string" },
    "image_url": { "value": "asia-northeast1-docker.pkg.dev/p/r/app:v1", "type": "string" }
  },
  "resources": [
    {
      "mode": "managed",
      "type": "google_sql_database_instance",
      "name": "main",
      "provider": "provider[\"registry.terraform.io/hashicorp/google\"]",
      "instances": [
        { "attributes": { "private_ip_address": "10.1.2.3" } }
      ]
    }
  ]
}`

func writeFixture(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "terraform.tfstate")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestRenderResolvesDefaultState(t *testing.T) {
	path := writeFixture(t, tfstateFixture)
	manifest := []byte(`serviceAccountName: '{{ tfstate "output.service_account" }}'
image: '{{ tfstate "output.image_url" }}'
dbHost: '{{ tfstate "google_sql_database_instance.main.private_ip_address" }}'`)

	out, err := Render(context.Background(), manifest, []Source{{Name: "default", Location: path}})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	got := string(out)
	for _, want := range []string{
		"serviceAccountName: 'run-sa@example.iam.gserviceaccount.com'",
		"image: 'asia-northeast1-docker.pkg.dev/p/r/app:v1'",
		"dbHost: '10.1.2.3'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Render() output missing %q:\n%s", want, got)
		}
	}
}

func TestRenderResolvesNamedState(t *testing.T) {
	path := writeFixture(t, tfstateFixture)
	manifest := []byte(`image: '{{ tfstate "network" "output.image_url" }}'`)

	out, err := Render(context.Background(), manifest, []Source{{Name: "network", Location: path}})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(string(out), "asia-northeast1-docker.pkg.dev/p/r/app:v1") {
		t.Errorf("named state not resolved:\n%s", out)
	}
}

func TestRenderNoPlaceholdersNeedsNoState(t *testing.T) {
	// state を一切渡さなくても、プレースホルダーが無ければ成功する (遅延ロード)。
	manifest := []byte("kind: Service\nmetadata:\n  name: svc\n")
	out, err := Render(context.Background(), manifest, nil)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if string(out) != string(manifest) {
		t.Errorf("Render() changed manifest without placeholders:\n%s", out)
	}
}

func TestRenderErrors(t *testing.T) {
	path := writeFixture(t, tfstateFixture)

	tests := []struct {
		name     string
		manifest string
		sources  []Source
		wantErr  string
	}{
		{
			name:     "unknown state name",
			manifest: `x: '{{ tfstate "missing" "output.image_url" }}'`,
			sources:  []Source{{Name: "default", Location: path}},
			wantErr:  `tfstate "missing" is not configured`,
		},
		{
			name:     "missing address",
			manifest: `x: '{{ tfstate "output.does_not_exist" }}'`,
			sources:  []Source{{Name: "default", Location: path}},
			wantErr:  "not found in tfstate",
		},
		{
			name:     "wrong arg count",
			manifest: `x: '{{ tfstate }}'`,
			sources:  []Source{{Name: "default", Location: path}},
			wantErr:  "tfstate requires 1",
		},
		{
			name:     "bad state location",
			manifest: `x: '{{ tfstate "output.image_url" }}'`,
			sources:  []Source{{Name: "default", Location: "/no/such/terraform.tfstate"}},
			wantErr:  "failed to read tfstate",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Render(context.Background(), []byte(tt.manifest), tt.sources)
			if err == nil {
				t.Fatalf("Render() = nil error, want %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Render() error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}
