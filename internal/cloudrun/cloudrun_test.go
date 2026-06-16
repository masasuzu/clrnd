package cloudrun

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/api/googleapi"
	run "google.golang.org/api/run/v1"
)

const validManifest = `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      - image: gcr.io/project/image:tag
`

func TestValidate(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		service  string
		wantErr  string // 期待するエラー部分文字列。空なら成功 (nil) を期待。
	}{
		{
			name:     "valid",
			manifest: validManifest,
			service:  "my-svc",
			wantErr:  "",
		},
		{
			name:     "service name mismatch",
			manifest: validManifest,
			service:  "other",
			wantErr:  "does not match",
		},
		{
			name: "wrong apiVersion",
			manifest: `apiVersion: v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      - image: gcr.io/x/y
`,
			service: "my-svc",
			wantErr: "apiVersion must be",
		},
		{
			name: "wrong kind",
			manifest: `apiVersion: serving.knative.dev/v1
kind: Deployment
metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      - image: gcr.io/x/y
`,
			service: "my-svc",
			wantErr: "kind must be",
		},
		{
			name: "missing metadata.name",
			manifest: `apiVersion: serving.knative.dev/v1
kind: Service
spec:
  template:
    spec:
      containers:
      - image: gcr.io/x/y
`,
			service: "my-svc",
			wantErr: "metadata.name is required",
		},
		{
			name: "no containers",
			manifest: `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec: {}
`,
			service: "my-svc",
			wantErr: "at least one container",
		},
		{
			name: "missing image",
			manifest: `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      - {}
`,
			service: "my-svc",
			wantErr: "image is required",
		},
		{
			// #1 で修正した nil パニックの回帰テスト。
			name: "null container",
			manifest: `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      - null
`,
			service: "my-svc",
			wantErr: "must not be null",
		},
		{
			// UnmarshalStrict が未知フィールド (typo) を検出する。
			name: "unknown field",
			manifest: `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
spec:
  template:
    spec:
      containerConcurency: 80
      containers:
      - image: gcr.io/x/y
`,
			service: "my-svc",
			wantErr: "unknown field",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate([]byte(tt.manifest), tt.service)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestValidateReportsMultipleProblems(t *testing.T) {
	manifest := `metadata:
  name: my-svc
spec:
  template:
    spec:
      containers:
      - {}
`
	err := Validate([]byte(manifest), "my-svc")
	if err == nil {
		t.Fatal("Validate() = nil, want aggregated errors")
	}
	for _, want := range []string{"apiVersion must be", "kind must be", "image is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregated error %q missing %q", err.Error(), want)
		}
	}
}

func TestToManifestStripsServerManagedFields(t *testing.T) {
	svc := &run.Service{
		ApiVersion: "serving.knative.dev/v1",
		Kind:       "Service",
		Metadata: &run.ObjectMeta{
			Name:            "my-svc",
			Namespace:       "123456789",
			Uid:             "abc-uid",
			ResourceVersion: "rv-1",
			Generation:      2,
			Annotations: map[string]string{
				"run.googleapis.com/operation-id": "op-1",
				"run.googleapis.com/ingress":      "all",
			},
		},
		Status: &run.ServiceStatus{ObservedGeneration: 2},
	}

	out, err := ToManifest(svc)
	if err != nil {
		t.Fatalf("ToManifest() error = %v", err)
	}
	got := string(out)

	for _, stripped := range []string{"status", "abc-uid", "rv-1", "operation-id", "namespace", "observedGeneration"} {
		if strings.Contains(got, stripped) {
			t.Errorf("ToManifest() output should not contain %q:\n%s", stripped, got)
		}
	}
	for _, kept := range []string{"name: my-svc", "run.googleapis.com/ingress: all"} {
		if !strings.Contains(got, kept) {
			t.Errorf("ToManifest() output should contain %q:\n%s", kept, got)
		}
	}
}

func TestNormalizeStripsServerManagedFields(t *testing.T) {
	manifest := `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: my-svc
  uid: abc-uid
  namespace: "123"
  annotations:
    run.googleapis.com/operation-id: op-1
    run.googleapis.com/ingress: all
status:
  observedGeneration: 2
`
	out, err := Normalize([]byte(manifest))
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	got := string(out)

	for _, stripped := range []string{"status", "abc-uid", "operation-id", "namespace", "observedGeneration"} {
		if strings.Contains(got, stripped) {
			t.Errorf("Normalize() output should not contain %q:\n%s", stripped, got)
		}
	}
	if !strings.Contains(got, "run.googleapis.com/ingress: all") {
		t.Errorf("Normalize() should keep non-managed annotations:\n%s", got)
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	manifest := []byte(validManifest)
	first, err := Normalize(manifest)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	second, err := Normalize(first)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("Normalize is not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestDiff(t *testing.T) {
	a := []byte("metadata:\n  name: svc\nimage: foo\n")
	b := []byte("metadata:\n  name: svc\nimage: bar\n")

	t.Run("identical returns empty", func(t *testing.T) {
		out, err := Diff(a, a, "live", "local")
		if err != nil {
			t.Fatalf("Diff() error = %v", err)
		}
		if out != "" {
			t.Errorf("Diff() of identical input = %q, want empty", out)
		}
	})

	t.Run("difference is shown with markers", func(t *testing.T) {
		out, err := Diff(a, b, "live", "local")
		if err != nil {
			t.Fatalf("Diff() error = %v", err)
		}
		if !strings.Contains(out, "-image: foo") {
			t.Errorf("Diff() missing removed line:\n%s", out)
		}
		if !strings.Contains(out, "+image: bar") {
			t.Errorf("Diff() missing added line:\n%s", out)
		}
		if !strings.Contains(out, "live") || !strings.Contains(out, "local") {
			t.Errorf("Diff() missing file labels:\n%s", out)
		}
	})
}

func TestServiceContainers(t *testing.T) {
	t.Run("nil-safe on empty service", func(t *testing.T) {
		if got := serviceContainers(&run.Service{}); got != nil {
			t.Errorf("serviceContainers(empty) = %v, want nil", got)
		}
	})

	t.Run("returns containers", func(t *testing.T) {
		svc := &run.Service{
			Spec: &run.ServiceSpec{
				Template: &run.RevisionTemplate{
					Spec: &run.RevisionSpec{
						Containers: []*run.Container{{Image: "x"}},
					},
				},
			},
		}
		got := serviceContainers(svc)
		if len(got) != 1 || got[0].Image != "x" {
			t.Errorf("serviceContainers() = %v, want one container with image x", got)
		}
	})
}

func TestIsNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"404 googleapi", &googleapi.Error{Code: 404}, true},
		{"wrapped 404", fmt.Errorf("check: %w", &googleapi.Error{Code: 404}), true},
		{"403 googleapi", &googleapi.Error{Code: 403}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNotFound(tt.err); got != tt.want {
				t.Errorf("isNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestDeleteMapKeys(t *testing.T) {
	t.Run("removes keys and drops empty parent field", func(t *testing.T) {
		parent := map[string]interface{}{
			"annotations": map[string]interface{}{"a": "1", "b": "2"},
		}
		deleteMapKeys(parent, "annotations", []string{"a", "b"})
		if _, ok := parent["annotations"]; ok {
			t.Errorf("empty annotations map should have been removed: %v", parent)
		}
	})

	t.Run("keeps non-empty parent field", func(t *testing.T) {
		parent := map[string]interface{}{
			"annotations": map[string]interface{}{"a": "1", "keep": "2"},
		}
		deleteMapKeys(parent, "annotations", []string{"a"})
		ann, ok := parent["annotations"].(map[string]interface{})
		if !ok {
			t.Fatalf("annotations should remain: %v", parent)
		}
		if _, ok := ann["keep"]; !ok {
			t.Errorf("non-target key should be kept: %v", ann)
		}
	})

	t.Run("missing field is a no-op", func(t *testing.T) {
		parent := map[string]interface{}{"other": 1}
		deleteMapKeys(parent, "annotations", []string{"a"})
		if len(parent) != 1 {
			t.Errorf("unexpected mutation: %v", parent)
		}
	})
}
