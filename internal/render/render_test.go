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
	// 名前付き state は名前をプレフィックスにした関数 ({{ <name>tfstate }}) になる。
	manifest := []byte(`image: '{{ network_tfstate "output.image_url" }}'`)

	out, err := Render(context.Background(), manifest, []Source{{Name: "network_", Location: path}})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(string(out), "asia-northeast1-docker.pkg.dev/p/r/app:v1") {
		t.Errorf("named state not resolved:\n%s", out)
	}
}

func TestRenderTfstatef(t *testing.T) {
	path := writeFixture(t, tfstateFixture)
	manifest := []byte(`a: '{{ tfstatef "output.%s" "image_url" }}'
b: '{{ prod_tfstatef "output.%s" "service_account" }}'`)

	out, err := Render(context.Background(), manifest, []Source{
		{Name: "default", Location: path},
		{Name: "prod_", Location: path},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	got := string(out)
	for _, want := range []string{
		"a: 'asia-northeast1-docker.pkg.dev/p/r/app:v1'",
		"b: 'run-sa@example.iam.gserviceaccount.com'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("tfstatef output missing %q:\n%s", want, got)
		}
	}
}

func TestRenderSingleQuoteAddr(t *testing.T) {
	path := writeFixture(t, tfstateFixture)
	// アドレス中の ' は " に置換される (ecspresso 互換)。ここでは置換しても
	// 同じアドレスに解決されることだけ確認する。
	manifest := []byte(`x: '{{ tfstate "output.image_url" }}'`)
	out, err := Render(context.Background(), manifest, []Source{{Name: "default", Location: path}})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(string(out), "asia-northeast1-docker.pkg.dev/p/r/app:v1") {
		t.Errorf("got %s", out)
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

func TestRenderEnv(t *testing.T) {
	t.Run("env uses value when set", func(t *testing.T) {
		t.Setenv("CLRND_TEST_VAR", "from-env")
		out, err := Render(context.Background(), []byte(`x: '{{ env "CLRND_TEST_VAR" "fallback" }}'`), nil)
		if err != nil {
			t.Fatalf("Render() error = %v", err)
		}
		if !strings.Contains(string(out), "x: 'from-env'") {
			t.Errorf("got %s", out)
		}
	})

	t.Run("env falls back to default when empty", func(t *testing.T) {
		t.Setenv("CLRND_TEST_VAR", "")
		out, err := Render(context.Background(), []byte(`x: '{{ env "CLRND_TEST_VAR" "fallback" }}'`), nil)
		if err != nil {
			t.Fatalf("Render() error = %v", err)
		}
		if !strings.Contains(string(out), "x: 'fallback'") {
			t.Errorf("got %s", out)
		}
	})

	t.Run("env without default yields empty", func(t *testing.T) {
		out, err := Render(context.Background(), []byte(`x: '{{ env "CLRND_UNSET_VAR" }}'`), nil)
		if err != nil {
			t.Fatalf("Render() error = %v", err)
		}
		if !strings.Contains(string(out), "x: ''") {
			t.Errorf("got %s", out)
		}
	})

	t.Run("must_env returns value when set", func(t *testing.T) {
		t.Setenv("CLRND_TEST_VAR", "present")
		out, err := Render(context.Background(), []byte(`x: '{{ must_env "CLRND_TEST_VAR" }}'`), nil)
		if err != nil {
			t.Fatalf("Render() error = %v", err)
		}
		if !strings.Contains(string(out), "x: 'present'") {
			t.Errorf("got %s", out)
		}
	})

	t.Run("must_env errors when undefined", func(t *testing.T) {
		_, err := Render(context.Background(), []byte(`x: '{{ must_env "CLRND_UNSET_VAR" }}'`), nil)
		if err == nil || !strings.Contains(err.Error(), "is not defined") {
			t.Fatalf("Render() error = %v, want 'is not defined'", err)
		}
	})
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
			name:     "unconfigured prefix is a parse error",
			manifest: `x: '{{ missing_tfstate "output.image_url" }}'`,
			sources:  []Source{{Name: "default", Location: path}},
			wantErr:  "function \"missing_tfstate\" not defined",
		},
		{
			name:     "missing address",
			manifest: `x: '{{ tfstate "output.does_not_exist" }}'`,
			sources:  []Source{{Name: "default", Location: path}},
			wantErr:  "not found in tfstate",
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
