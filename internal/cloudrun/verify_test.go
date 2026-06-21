package cloudrun

import (
	"reflect"
	"testing"

	run "google.golang.org/api/run/v1"
)

func TestSecretResourceName(t *testing.T) {
	aliases := map[string]string{
		"db_pass":    "projects/other-proj/secrets/db-password",
		"with_ver":   "projects/other-proj/secrets/api-key/versions/3",
		"short_only": "shorthand", // 異常系: projects/ 接頭辞なし
	}
	tests := []struct {
		name    string
		project string
		secret  string
		want    string
	}{
		{"same-project short name", "p", "my-secret", "projects/p/secrets/my-secret"},
		{"already qualified", "p", "projects/q/secrets/s", "projects/q/secrets/s"},
		{"qualified with version stripped", "p", "projects/q/secrets/s/versions/5", "projects/q/secrets/s"},
		{"cross-project alias resolved", "p", "db_pass", "projects/other-proj/secrets/db-password"},
		{"cross-project alias with version stripped", "p", "with_ver", "projects/other-proj/secrets/api-key"},
		{"unknown alias falls back to same project", "p", "missing", "projects/p/secrets/missing"},
		{"alias target not qualified falls back", "p", "short_only", "projects/p/secrets/shorthand"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := secretResourceName(tt.project, tt.secret, aliases); got != tt.want {
				t.Errorf("secretResourceName(%q, %q) = %q, want %q", tt.project, tt.secret, got, tt.want)
			}
		})
	}
}

func TestSecretAliases(t *testing.T) {
	t.Run("nil-safe on empty service", func(t *testing.T) {
		if got := secretAliases(&run.Service{}); got != nil {
			t.Errorf("secretAliases(empty) = %v, want nil", got)
		}
	})

	t.Run("parses comma-separated aliases", func(t *testing.T) {
		svc := &run.Service{
			Spec: &run.ServiceSpec{
				Template: &run.RevisionTemplate{
					Metadata: &run.ObjectMeta{
						Annotations: map[string]string{
							secretAliasAnnotation: "a:projects/p1/secrets/s1, b:projects/p2/secrets/s2",
						},
					},
				},
			},
		}
		got := secretAliases(svc)
		want := map[string]string{
			"a": "projects/p1/secrets/s1",
			"b": "projects/p2/secrets/s2",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("secretAliases() = %v, want %v", got, want)
		}
	})
}

func TestSecretNames(t *testing.T) {
	svc := &run.Service{
		Spec: &run.ServiceSpec{
			Template: &run.RevisionTemplate{
				Spec: &run.RevisionSpec{
					Containers: []*run.Container{{
						Env: []*run.EnvVar{
							{Name: "A", Value: "plain"},
							{Name: "B", ValueFrom: &run.EnvVarSource{SecretKeyRef: &run.SecretKeySelector{Name: "s1", Key: "latest"}}},
							{Name: "C", ValueFrom: &run.EnvVarSource{SecretKeyRef: &run.SecretKeySelector{Name: "s1", Key: "1"}}}, // 重複
						},
					}},
					Volumes: []*run.Volume{
						{Name: "v", Secret: &run.SecretVolumeSource{SecretName: "s2"}},
					},
				},
			},
		},
	}
	got := secretNames(svc)
	want := []string{"s1", "s2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("secretNames() = %v, want %v (deduped, env + volume)", got, want)
	}
}
