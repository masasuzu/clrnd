package cloudrun

import (
	"strings"
	"testing"
)

const twoContainerManifest = `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      - name: app
        image: gcr.io/p/app:v1
      - name: proxy
        image: gcr.io/p/proxy:v1
`

func TestParseImageOverride(t *testing.T) {
	tests := []struct {
		name      string
		spec      string
		container string
		image     string
		wantErr   string
	}{
		{name: "image only", spec: "gcr.io/p/app:v2", image: "gcr.io/p/app:v2"},
		{name: "with container", spec: "app=gcr.io/p/app:v2", container: "app", image: "gcr.io/p/app:v2"},
		{
			name:      "digest keeps its colons and at-sign",
			spec:      "app=us-docker.pkg.dev/p/r/app@sha256:abc",
			container: "app", image: "us-docker.pkg.dev/p/r/app@sha256:abc",
		},
		{name: "empty", spec: "  ", wantErr: "needs a value"},
		{name: "no image", spec: "app=", wantErr: "has no image"},
		{name: "no container name", spec: "=gcr.io/p/app:v2", wantErr: "no container name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseImageOverride(tt.spec)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseImageOverride(%q) error = %v, want it to mention %q", tt.spec, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseImageOverride(%q) error = %v", tt.spec, err)
			}
			if got.Container != tt.container || got.Image != tt.image {
				t.Errorf("ParseImageOverride(%q) = %+v, want {%q %q}", tt.spec, got, tt.container, tt.image)
			}
		})
	}
}

// TestApplyImageOverridesSingleContainer checks that the name can be omitted when there is only
// one container.
func TestApplyImageOverridesSingleContainer(t *testing.T) {
	got, err := ApplyImageOverrides([]byte(validManifest), []string{"gcr.io/p/app:v2"})
	if err != nil {
		t.Fatalf("ApplyImageOverrides() error = %v", err)
	}
	svc, err := parseManifest(got)
	if err != nil {
		t.Fatalf("the result does not parse: %v", err)
	}
	containers := serviceContainers(svc)
	if len(containers) != 1 || containers[0].Image != "gcr.io/p/app:v2" {
		t.Errorf("containers = %+v, want the image replaced", containers)
	}
	// Everything other than the replacement is preserved.
	if svc.Metadata == nil || svc.Metadata.Name != "my-svc" {
		t.Errorf("metadata = %+v, want the rest of the manifest kept", svc.Metadata)
	}
}

// TestApplyImageOverridesNeedsANameWithSidecars checks that an override with the name omitted is
// refused when there is more than one container. Silently rewriting the first one replaces the
// wrong container in a service that has sidecars.
func TestApplyImageOverridesNeedsANameWithSidecars(t *testing.T) {
	_, err := ApplyImageOverrides([]byte(twoContainerManifest), []string{"gcr.io/p/app:v2"})
	if err == nil {
		t.Fatal("ApplyImageOverrides() error = nil, want it to refuse an ambiguous override")
	}
	for _, want := range []string{"2 containers", "app", "proxy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

// TestApplyImageOverridesByName checks that naming a container changes only that container.
func TestApplyImageOverridesByName(t *testing.T) {
	got, err := ApplyImageOverrides([]byte(twoContainerManifest), []string{"proxy=gcr.io/p/proxy:v2"})
	if err != nil {
		t.Fatalf("ApplyImageOverrides() error = %v", err)
	}
	svc, err := parseManifest(got)
	if err != nil {
		t.Fatalf("the result does not parse: %v", err)
	}
	containers := serviceContainers(svc)
	if len(containers) != 2 {
		t.Fatalf("containers = %+v, want both kept", containers)
	}
	if containers[0].Image != "gcr.io/p/app:v1" {
		t.Errorf("containers[0].image = %q, want the untouched container left alone", containers[0].Image)
	}
	if containers[1].Image != "gcr.io/p/proxy:v2" {
		t.Errorf("containers[1].image = %q, want the named container replaced", containers[1].Image)
	}
}

// TestApplyImageOverridesRejectsAnUnknownContainer checks that a container name that does not
// exist is rejected. Letting it through means the deploy proceeds with "specified, yet not taking
// effect".
func TestApplyImageOverridesRejectsAnUnknownContainer(t *testing.T) {
	_, err := ApplyImageOverrides([]byte(twoContainerManifest), []string{"sidecar=gcr.io/p/x:v2"})
	if err == nil || !strings.Contains(err.Error(), "does not define") {
		t.Errorf("ApplyImageOverrides() error = %v, want an unknown container to be rejected", err)
	}
}

// TestApplyImageOverridesWithoutSpecsIsAPassthrough checks that, with no overrides given, the
// manifest is returned as is (not reformatted).
func TestApplyImageOverridesWithoutSpecsIsAPassthrough(t *testing.T) {
	got, err := ApplyImageOverrides([]byte(validManifest), nil)
	if err != nil {
		t.Fatalf("ApplyImageOverrides() error = %v", err)
	}
	if string(got) != validManifest {
		t.Errorf("ApplyImageOverrides() = %q, want the manifest untouched", got)
	}
}

// TestApplyImageOverridesRejectsANullContainer checks that a manifest containing a null container
// does not crash it (no panic). Validate rejects this, but the override runs before it, so
// without a check here it assigns to nil and panics.
func TestApplyImageOverridesRejectsANullContainer(t *testing.T) {
	const manifest = `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      -
`
	_, err := ApplyImageOverrides([]byte(manifest), []string{"gcr.io/p/app:v2"})
	if err == nil {
		t.Fatal("ApplyImageOverrides() error = nil, want the null container rejected")
	}
	// Use the same wording as Validate (so the explanation does not change with or without
	// --image).
	if !strings.Contains(err.Error(), "containers[0] must not be null") {
		t.Errorf("error = %v, want the same message Validate gives", err)
	}
}
