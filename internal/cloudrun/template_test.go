package cloudrun

import (
	"testing"

	run "google.golang.org/api/run/v1"
)

func TestWithRevisionNameDoesNotModifyItsArgument(t *testing.T) {
	tmpl := &run.RevisionTemplate{
		Metadata: &run.ObjectMeta{Name: "my-svc-00001", Annotations: map[string]string{"a": "1"}},
		Spec:     &run.RevisionSpec{ServiceAccountName: "sa"},
	}
	got := withRevisionName(tmpl, "my-svc-r1")
	if revisionNameOf(got) != "my-svc-r1" || revisionNameOf(tmpl) != "my-svc-00001" {
		t.Errorf("withRevisionName: got %q, original now %q", revisionNameOf(got), revisionNameOf(tmpl))
	}
	if got.Spec != tmpl.Spec || got.Metadata.Annotations["a"] != "1" {
		t.Error("withRevisionName lost the rest of the template")
	}
	if revisionNameOf(withRevisionName(tmpl, "")) != "" {
		t.Error(`withRevisionName(t, "") kept the name`)
	}
	// A template with no metadata, or none at all, still gets one.
	if revisionNameOf(withRevisionName(nil, "x")) != "x" || revisionNameOf(withRevisionName(&run.RevisionTemplate{}, "x")) != "x" {
		t.Error("withRevisionName did not set a name on an empty template")
	}
}

func TestTemplateWorkload(t *testing.T) {
	if w := templateWorkload("spec.template.spec", nil); w.Path != "spec.template.spec" || w.Containers != nil {
		t.Errorf("templateWorkload(nil) = %+v", w)
	}
	tmpl := &run.RevisionTemplate{
		Metadata: &run.ObjectMeta{Annotations: map[string]string{"run.googleapis.com/vpc-access-connector": "c"}},
		Spec: &run.RevisionSpec{
			Containers:         []*run.Container{{Image: "img"}},
			ServiceAccountName: "sa@example.iam.gserviceaccount.com",
		},
	}
	w := templateWorkload("spec.template.spec", tmpl)
	if len(w.Containers) != 1 || w.ServiceAccountName == "" || w.annotation("run.googleapis.com/vpc-access-connector") != "c" {
		t.Errorf("templateWorkload() = %+v", w)
	}
}
