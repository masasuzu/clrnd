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

// A minimal Terraform state v4 fixture. It contains outputs and resource attributes.
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
	// A named state becomes functions prefixed with its name ({{ <name>tfstate }}).
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
	// A ' in the address is replaced with " (ecspresso-compatible). This only checks that the
	// address still resolves to the same one with the replacement applied.
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
	// The name becomes a function name (<name>tfstate), so a name that is not a valid Go
	// identifier is rejected with a clean error rather than a panic (an invalid name coming
	// through the config path must not crash).
	for _, name := range []string{"net-prod", "1state", "has space"} {
		manifest := []byte("kind: Service\n")
		_, err := Render(context.Background(), manifest, []Source{{Name: name, Location: path}})
		if err == nil || !strings.Contains(err.Error(), "invalid tfstate name") {
			t.Errorf("Render() with name %q error = %v, want 'invalid tfstate name'", name, err)
		}
	}
}

func TestRenderNoPlaceholdersNeedsNoState(t *testing.T) {
	// With no placeholders, rendering succeeds even when no state is passed at all (lazy loading).
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

// TestRenderJSONEscape checks the escaping of a value embedded in JSON. The function comes from
// ecspresso and is needed when writing JSON in an annotation or env[].value.
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

// TestRenderJSONEscapeAcceptsNonStrings checks that passing something other than a string does
// not break it (tfstate can also return numbers and booleans).
func TestRenderJSONEscapeAcceptsNonStrings(t *testing.T) {
	got, err := Render(context.Background(), []byte(`n: "{{ 42 | json_escape }}"`), nil)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if string(got) != `n: "42"` {
		t.Errorf("Render() = %q, want the value rendered as a JSON string body", got)
	}
}

// TestRenderJSONEscapeKeepsHTMLCharactersLiteral checks that & < > are not turned into a form
// like \u0026. As JSON it is equivalent, but a reader that takes the value as is (an annotation or
// env[].value not re-parsed as JSON) sees it garbled.
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

// TestRenderJSONEscapeRejectsInvalidUTF8 checks that invalid UTF-8 is not silently replaced.
// json.Marshal does not return an error but squashes it to U+FFFD, so left as is, a mangled
// value would be deployed.
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

// TestRenderJSONEscapeInABlockScalar checks the form the README recommends (a >- block scalar)
// end to end, with a value that contains an apostrophe.
//
// json_escape escapes for JSON, so ' is not covered, and embedding the value in a '...' YAML
// scalar breaks the YAML for some values (change this template to '...' and this test really does
// fail while parsing the YAML). By reading the rendered output as YAML and then parsing the
// extracted string as JSON, it checks the YAML layer and the JSON layer alike.
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

	// YAML layer: the output can be read as a document.
	var doc struct {
		Note string `json:"note"`
	}
	if err := yaml.Unmarshal(rendered, &doc); err != nil {
		t.Fatalf("the rendered manifest is not valid YAML: %v\n%s", err, rendered)
	}

	// JSON layer: the extracted string can be read as JSON and gives back the original value.
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
