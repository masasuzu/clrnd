package cloudrun

import (
	"fmt"
	"strings"

	run "google.golang.org/api/run/v1"
	"sigs.k8s.io/yaml"
)

// ImageOverride is a single --image value. An empty Container means "the only container in the
// manifest".
type ImageOverride struct {
	Container string
	Image     string
}

// ParseImageOverride parses an --image value. The form is "<image>" or "<container>=<image>".
//
// Splitting at the first "=" is unambiguous: "=" appears in neither a container name nor an
// image reference (host/path:tag@digest).
func ParseImageOverride(spec string) (ImageOverride, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ImageOverride{}, fmt.Errorf("--image needs a value: <image> or <container>=<image>")
	}
	name, image := "", spec
	if i := strings.Index(spec, "="); i >= 0 {
		name, image = strings.TrimSpace(spec[:i]), strings.TrimSpace(spec[i+1:])
	}
	if image == "" {
		return ImageOverride{}, fmt.Errorf("--image %q has no image", spec)
	}
	if strings.Contains(spec, "=") && name == "" {
		return ImageOverride{}, fmt.Errorf("--image %q has no container name before the '='", spec)
	}
	return ImageOverride{Container: name, Image: image}, nil
}

// ApplyImageOverrides returns the manifest with containers[].image replaced.
// With no overrides it returns the manifest unchanged (so nothing is reformatted when the flag is
// not used).
//
// An override is an exception to the principle that "the manifest is the only input", so its
// reach is kept narrow: the container name may be omitted only when there is one container, and a
// service with several has to say which container. Silently rewriting the first one would, on a
// service with a sidecar, replace a container that was never meant to change.
func ApplyImageOverrides(manifest []byte, specs []string) ([]byte, error) {
	if len(specs) == 0 {
		return manifest, nil
	}
	overrides := make([]ImageOverride, 0, len(specs))
	for _, spec := range specs {
		o, err := ParseImageOverride(spec)
		if err != nil {
			return nil, err
		}
		overrides = append(overrides, o)
	}

	svc, err := parseManifest(manifest)
	if err != nil {
		return nil, err
	}
	containers := serviceContainers(svc)
	if len(containers) == 0 {
		return nil, fmt.Errorf("--image needs a container to apply to, but the manifest defines none")
	}
	// A null container (a manifest with a bare "-" under containers:) is rejected by Validate,
	// but the override runs before that. Without checking here, it would assign to nil and panic.
	// The message matches Validate's, so the explanation does not depend on whether --image was
	// passed.
	for i, c := range containers {
		if c == nil {
			return nil, fmt.Errorf("spec.template.spec.containers[%d] must not be null", i)
		}
	}

	for _, o := range overrides {
		target, err := findContainer(containers, o.Container)
		if err != nil {
			return nil, err
		}
		target.Image = o.Image
	}

	out, err := yaml.Marshal(svc)
	if err != nil {
		return nil, fmt.Errorf("failed to rebuild the manifest after --image: %w", err)
	}
	return out, nil
}

// findContainer looks a container up by name. An empty name succeeds only when there is exactly
// one container.
func findContainer(containers []*run.Container, name string) (*run.Container, error) {
	if name == "" {
		if len(containers) != 1 {
			return nil, fmt.Errorf(
				"--image needs a container name because the manifest defines %d containers (%s); "+
					"use --image <container>=<image>", len(containers), strings.Join(containerNames(containers), ", "))
		}
		return containers[0], nil
	}
	for _, c := range containers {
		if c != nil && c.Name == name {
			return c, nil
		}
	}
	return nil, fmt.Errorf("--image names container %q, which the manifest does not define (%s)",
		name, strings.Join(containerNames(containers), ", "))
}

// containerNames lists the container names to put in an error. A container with no name is
// "<unnamed>".
func containerNames(containers []*run.Container) []string {
	out := make([]string, 0, len(containers))
	for _, c := range containers {
		switch {
		case c == nil:
			continue
		case c.Name == "":
			out = append(out, "<unnamed>")
		default:
			out = append(out, c.Name)
		}
	}
	return out
}
