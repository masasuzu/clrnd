package render

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
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

func TestRenderRejectsInvalidName(t *testing.T) {
	path := writeFixture(t, tfstateFixture)
	// 名前は関数名 (<name>tfstate) になるため、Go 識別子として不正な名前は panic ではなく
	// クリーンなエラーで弾く (config 経路から不正名が来ても落ちないこと)。
	for _, name := range []string{"net-prod", "1state", "has space"} {
		manifest := []byte("kind: Service\n")
		_, err := Render(context.Background(), manifest, []Source{{Name: name, Location: path}})
		if err == nil || !strings.Contains(err.Error(), "invalid tfstate name") {
			t.Errorf("Render() with name %q error = %v, want 'invalid tfstate name'", name, err)
		}
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

// TestRenderJSONEscape は、JSON に埋める値のエスケープを確認する。ecspresso にある
// 関数で、アノテーションや env[].value に JSON を書くときに要る。
func TestRenderJSONEscape(t *testing.T) {
	t.Setenv("CONFIG_JSON", `he said "hi"`+"\n\tdone\\")

	got, err := Render(context.Background(),
		[]byte(`value: '{"note": "{{ must_env "CONFIG_JSON" | json_escape }}"}'`), nil)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	want := `value: '{"note": "he said \"hi\"\n\tdone\\"}'`
	if string(got) != want {
		t.Errorf("Render() = %q, want %q", got, want)
	}
}

// TestRenderJSONEscapeAcceptsNonStrings は、文字列以外を渡しても壊れないことを
// 確認する (tfstate は数値や真偽値も返しうる)。
func TestRenderJSONEscapeAcceptsNonStrings(t *testing.T) {
	got, err := Render(context.Background(), []byte(`n: "{{ 42 | json_escape }}"`), nil)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if string(got) != `n: "42"` {
		t.Errorf("Render() = %q, want the value rendered as a JSON string body", got)
	}
}

// TestRenderJSONEscapeKeepsHTMLCharactersLiteral は、& < > を & のような形に
// しないことを確認する。JSON としては同じだが、値をそのまま読む相手 (JSON として
// 再パースされないアノテーションや env[].value) には化けて見える。
func TestRenderJSONEscapeKeepsHTMLCharactersLiteral(t *testing.T) {
	t.Setenv("QUERY", "a=1&b<2>3")

	got, err := Render(context.Background(), []byte(`v: '{{ must_env "QUERY" | json_escape }}'`), nil)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if want := `v: 'a=1&b<2>3'`; string(got) != want {
		t.Errorf("Render() = %q, want %q", got, want)
	}
}

// TestRenderJSONEscapeRejectsInvalidUTF8 は、不正な UTF-8 を黙って置き換えないことを
// 確認する。json.Marshal はエラーにせず U+FFFD に潰すので、そのままだと化けた値が
// デプロイされる。
func TestRenderJSONEscapeRejectsInvalidUTF8(t *testing.T) {
	t.Setenv("BROKEN", string([]byte{0xff, 0xfe}))

	_, err := Render(context.Background(), []byte(`v: '{{ must_env "BROKEN" | json_escape }}'`), nil)
	if err == nil {
		t.Fatal("Render() error = nil, want the invalid UTF-8 rejected")
	}
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("Render() error = %v, want it to name the encoding problem", err)
	}
}

// TestRenderJSONEscapeInABlockScalar は、README が勧める書き方 (>- のブロックスカラー)
// を、アポストロフィを含む値で通しで確認する。
//
// json_escape は JSON 用のエスケープなので ' は対象外で、'...' の YAML スカラーに
// 埋めると値によっては YAML が壊れる (この template を '...' に変えると、実際に
// このテストは YAML のパースで落ちる)。展開結果を YAML として読み、取り出した文字列を
// さらに JSON としてパースすることで、YAML 層と JSON 層の両方を見る。
func TestRenderJSONEscapeInABlockScalar(t *testing.T) {
	const raw = `it's "quoted" & has
a newline`
	t.Setenv("CLRND_CONFIG", raw)

	const template = `note: >-
  {"text": "{{ must_env "CLRND_CONFIG" | json_escape }}"}
`
	rendered, err := Render(context.Background(), []byte(template), nil)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	// YAML 層: ドキュメントとして読めること。
	var doc struct {
		Note string `json:"note"`
	}
	if err := yaml.Unmarshal(rendered, &doc); err != nil {
		t.Fatalf("the rendered manifest is not valid YAML: %v\n%s", err, rendered)
	}

	// JSON 層: 取り出した文字列が JSON として読め、元の値に戻ること。
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(doc.Note), &payload); err != nil {
		t.Fatalf("the value is not valid JSON: %v\n%s", err, doc.Note)
	}
	if payload.Text != raw {
		t.Errorf("round-tripped value = %q, want %q", payload.Text, raw)
	}
}
