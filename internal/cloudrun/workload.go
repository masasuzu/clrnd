package cloudrun

import (
	"fmt"
	"strings"

	run "google.golang.org/api/run/v1"
)

// workload is what a manifest says will run: the containers, the volumes they mount, the identity
// they run as, and the template annotations Cloud Run reads alongside them.
//
// Every Cloud Run resource that runs containers holds these, but each keeps them at its own depth:
// a Service under spec.template, a Job one template further down. The checks that read them —
// verify's remote existence checks, --image, the container rules in validate — only care about the
// content, so they take a workload rather than a *run.Service. Supporting another kind then means
// writing its extractor, not another copy of every check.
type workload struct {
	// Path is where the containers' spec sits in the manifest (e.g. "spec.template.spec"), so that
	// an error names the field the user has to fix.
	Path string

	Containers         []*run.Container
	Volumes            []*run.Volume
	ServiceAccountName string

	// Annotations are the template's annotations, which is where Cloud Run reads the VPC connector,
	// the Cloud SQL instances and the aliases of secrets in another project.
	Annotations map[string]string
}

// serviceWorkload extracts a service's workload nil-safely. What the service does not define is
// left zero. The slices are shared with svc, so a change through Containers changes svc.
func serviceWorkload(svc *run.Service) workload {
	w := workload{Path: "spec.template.spec"}
	if spec := templateSpec(svc); spec != nil {
		w.Containers = spec.Containers
		w.Volumes = spec.Volumes
		w.ServiceAccountName = spec.ServiceAccountName
	}
	if meta := templateMeta(svc); meta != nil {
		w.Annotations = meta.Annotations
	}
	return w
}

// annotation reads a template annotation with surrounding whitespace removed.
func (w workload) annotation(key string) string {
	return strings.TrimSpace(w.Annotations[key])
}

// validateContainers checks what every kind requires of its containers: at least one, none of
// them null, each with an image.
func validateContainers(w workload) []error {
	var errs []error
	if len(w.Containers) == 0 {
		errs = append(errs, fmt.Errorf("%s.containers must define at least one container", w.Path))
	}
	for i, c := range w.Containers {
		switch {
		case c == nil:
			errs = append(errs, nullContainerError(w, i))
		case c.Image == "":
			errs = append(errs, fmt.Errorf("%s.containers[%d].image is required", w.Path, i))
		}
	}
	return errs
}

// nullContainerError reports a container written as a bare "-". validateContainers and --image
// both have to reject it, and share this so the message does not depend on whether --image was
// passed.
func nullContainerError(w workload, i int) error {
	return fmt.Errorf("%s.containers[%d] must not be null", w.Path, i)
}
