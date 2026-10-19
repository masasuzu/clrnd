//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

func (s *suite) render(t *testing.T) {
	t.Helper()
	dir := s.dir(t, "current-render")
	state := filepath.Join(dir, "e2e.tfstate")
	// Keep Terraform state as a fixture, rather than calling clrnd's internal renderer.
	b, err := json.Marshal(map[string]any{
		"version": 4, "terraform_version": "1.9.0", "serial": 1, "lineage": "clrnd-e2e", "outputs": map[string]any{},
		"resources": []any{map[string]any{"mode": "managed", "type": "null_resource", "name": "image", "provider": `provider["registry.terraform.io/hashicorp/null"]`,
			"instances": []any{map[string]any{"schema_version": 0, "attributes": map[string]any{"id": s.c.image}}}}},
	})
	if err != nil {
		s.fatal(t, "state JSON: %v", err)
	}
	s.write(t, state, b)
	template := filepath.Join(dir, "template.yaml")
	s.write(t, template, []byte(`apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: {{ must_env "CLRND_E2E_SERVICE" }}
spec:
  template:
    spec:
      containers:
      - image: {{ tfstate "null_resource.image.id" }}
        env:
        - name: FROM_ENV
          value: "{{ env "CLRND_E2E_UNSET" "fallback" }}"
`))
	env := map[string]string{"CLRND_E2E_SERVICE": s.service, "CLRND_E2E_UNSET": ""}
	invoke := func(args ...string) result {
		ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
		defer cancel()
		return s.command(t, ctx, dir, env, s.binary, args...)
	}
	r := s.code(t, invoke("render", template, "--tfstate", state), 0)
	s.contains(t, r.stdout, "image: "+s.c.image)
	s.contains(t, r.stdout, "name: "+s.service)
	s.contains(t, r.stdout, "fallback")
	s.lacks(t, r.stdout, "{{")
	rendered := filepath.Join(dir, "rendered.yaml")
	r = s.code(t, invoke("render", template, "--tfstate", state, "-o", rendered), 0)
	s.empty(t, r.stdout)
	s.contains(t, string(s.read(t, rendered)), "image: "+s.c.image)
	s.fails(t, invoke("render", template, "--tfstate", state, "-o", template))
	s.contains(t, string(s.read(t, template)), "must_env")
	raw := `it's "quoted" & fine`
	env["CLRND_E2E_RAW"] = raw
	escape := filepath.Join(dir, "escape.yaml")
	s.write(t, escape, []byte(fmt.Sprintf(`apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: %s
spec:
  template:
    metadata:
      annotations:
        clrnd-e2e.example.com/config: >-
          {"text": "{{ must_env "CLRND_E2E_RAW" | json_escape }}"}
    spec:
      containers:
      - image: %s
`, s.service, s.c.image)))
	escaped := filepath.Join(dir, "escaped.yaml")
	s.code(t, invoke("render", escape, "-o", escaped), 0)
	s.contains(t, string(s.read(t, escaped)), `\"quoted\"`)
	s.code(t, invoke("verify", s.service, escaped, "--local-only"), 0)
	var doc struct {
		Spec struct {
			Template struct {
				Metadata struct{ Annotations map[string]string }
			}
		}
	}
	if err = yaml.Unmarshal(s.read(t, escaped), &doc); err != nil {
		s.fatal(t, "escaped YAML: %v", err)
	}
	value := decode[struct{ Text string }](s, t, []byte(doc.Spec.Template.Metadata.Annotations["clrnd-e2e.example.com/config"]))
	if value.Text != raw {
		s.fatal(t, "json_escape did not round trip")
	}
	s.code(t, invoke("verify", s.service, template, "--tfstate", state, "--local-only"), 0)
}
