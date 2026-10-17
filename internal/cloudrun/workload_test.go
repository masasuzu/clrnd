package cloudrun

import (
	"reflect"
	"strings"
	"testing"

	run "google.golang.org/api/run/v1"
)

func TestServiceWorkload(t *testing.T) {
	t.Run("nil-safe on an empty service", func(t *testing.T) {
		for _, svc := range []*run.Service{nil, {}, {Spec: &run.ServiceSpec{}}} {
			w := serviceWorkload(svc)
			if w.Containers != nil || w.Volumes != nil || w.ServiceAccountName != "" || w.Annotations != nil {
				t.Errorf("serviceWorkload(%+v) = %+v, want only the path set", svc, w)
			}
			if w.Path != "spec.template.spec" {
				t.Errorf("Path = %q, want spec.template.spec", w.Path)
			}
		}
	})

	t.Run("reads the template", func(t *testing.T) {
		container := &run.Container{Image: "x"}
		volume := &run.Volume{Name: "v"}
		svc := &run.Service{
			Spec: &run.ServiceSpec{
				Template: &run.RevisionTemplate{
					Metadata: &run.ObjectMeta{Annotations: map[string]string{"k": " v "}},
					Spec: &run.RevisionSpec{
						Containers:         []*run.Container{container},
						Volumes:            []*run.Volume{volume},
						ServiceAccountName: "sa@p.iam.gserviceaccount.com",
					},
				},
			},
		}
		w := serviceWorkload(svc)
		if !reflect.DeepEqual(w.Containers, []*run.Container{container}) {
			t.Errorf("Containers = %v", w.Containers)
		}
		if !reflect.DeepEqual(w.Volumes, []*run.Volume{volume}) {
			t.Errorf("Volumes = %v", w.Volumes)
		}
		if w.ServiceAccountName != "sa@p.iam.gserviceaccount.com" {
			t.Errorf("ServiceAccountName = %q", w.ServiceAccountName)
		}
		if got := w.annotation("k"); got != "v" {
			t.Errorf("annotation(k) = %q, want the trimmed value", got)
		}
	})

	// --image relies on this: it edits the containers through the workload and then marshals svc.
	t.Run("shares the containers with the service", func(t *testing.T) {
		svc := &run.Service{Spec: &run.ServiceSpec{Template: &run.RevisionTemplate{
			Spec: &run.RevisionSpec{Containers: []*run.Container{{Image: "old"}}},
		}}}
		serviceWorkload(svc).Containers[0].Image = "new"
		if got := svc.Spec.Template.Spec.Containers[0].Image; got != "new" {
			t.Errorf("image after the change = %q, want new", got)
		}
	})
}

func TestValidateContainersNamesThePath(t *testing.T) {
	w := workload{Path: "spec.x", Containers: []*run.Container{nil, {}}}
	var msgs []string
	for _, err := range validateContainers(w) {
		msgs = append(msgs, err.Error())
	}
	want := []string{"spec.x.containers[0] must not be null", "spec.x.containers[1].image is required"}
	if !reflect.DeepEqual(msgs, want) {
		t.Errorf("validateContainers() = %q, want %q", msgs, want)
	}

	errs := validateContainers(workload{Path: "spec.x"})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "spec.x.containers must define at least one container") {
		t.Errorf("validateContainers(no containers) = %v", errs)
	}
}
