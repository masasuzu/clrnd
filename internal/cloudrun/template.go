package cloudrun

import run "google.golang.org/api/run/v1"

// The helpers here work on a RevisionTemplate, not on the resource holding it. A Service keeps one
// at spec.template, and so does a WorkerPool: everything about revisions — the name a manifest may
// pin, what runs in them — is the same for both, so it is written once against the template and
// each kind supplies only how to reach it (template) and how to put a changed one back
// (setTemplate). A Job has no revisions and no template of this type.

// revisionNameOf returns the revision name a template pins (metadata.name), nil-safely; "" when it
// pins none.
func revisionNameOf(t *run.RevisionTemplate) string {
	if t == nil || t.Metadata == nil {
		return ""
	}
	return t.Metadata.Name
}

// withRevisionName returns a copy of t whose revision name is name ("" removes it). It does not
// modify t: only the template and its metadata are copied.
func withRevisionName(t *run.RevisionTemplate, name string) *run.RevisionTemplate {
	out := run.RevisionTemplate{}
	if t != nil {
		out = *t
	}
	meta := run.ObjectMeta{}
	if out.Metadata != nil {
		meta = *out.Metadata
	}
	meta.Name = name
	out.Metadata = &meta
	return &out
}

// templateWorkload extracts what a template runs, nil-safely. path is where the template's spec
// sits in the resource, for messages. The slices are shared with t.
func templateWorkload(path string, t *run.RevisionTemplate) workload {
	w := workload{Path: path}
	if t == nil {
		return w
	}
	if spec := t.Spec; spec != nil {
		w.Containers = spec.Containers
		w.Volumes = spec.Volumes
		w.ServiceAccountName = spec.ServiceAccountName
	}
	if meta := t.Metadata; meta != nil {
		w.Annotations = meta.Annotations
	}
	return w
}

// withoutRevisionNameOf returns the scaffold hook of a kind whose resource holds a revision
// template: the resource without the revision name it pins (see WithoutRevisionName). It returns
// its argument unchanged when no name is pinned.
func withoutRevisionNameOf[T resource](template func(T) *run.RevisionTemplate,
	setTemplate func(T, *run.RevisionTemplate) T) func(T) T {
	return func(obj T) T {
		t := template(obj)
		if revisionNameOf(t) == "" {
			return obj
		}
		return setTemplate(obj, withRevisionName(t, ""))
	}
}

// alignRevisionNameOf returns the align hook of a kind whose resource holds a revision template:
// when desired pins no revision name, the live definition is compared without its own (see
// alignRevisionName).
func alignRevisionNameOf[T resource](template func(T) *run.RevisionTemplate,
	setTemplate func(T, *run.RevisionTemplate) T) func(current, desired T) T {
	without := withoutRevisionNameOf(template, setTemplate)
	return func(current, desired T) T {
		var zero T
		if current == zero || revisionNameOf(template(desired)) != "" {
			return current
		}
		return without(current)
	}
}
