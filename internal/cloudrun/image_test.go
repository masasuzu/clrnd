package cloudrun

import (
	"testing"

	run "google.golang.org/api/run/v1"
)

func TestParseImageRef(t *testing.T) {
	tests := []struct {
		name     string
		image    string
		wantAR   bool
		wantName string
		want     imageRef
	}{
		{
			name:     "artifact registry with a tag",
			image:    "asia-northeast1-docker.pkg.dev/my-project/my-repo/my-app:v1",
			wantAR:   true,
			wantName: "projects/my-project/locations/asia-northeast1/repositories/my-repo/packages/my-app/tags/v1",
		},
		{
			name:     "no tag defaults to latest",
			image:    "us-docker.pkg.dev/cloudrun/container/hello",
			wantAR:   true,
			wantName: "projects/cloudrun/locations/us/repositories/container/packages/hello/tags/latest",
		},
		{
			// API が返す名前もこの形。"/" 以外をエスケープすると 404 になる (実 API で確認済み)。
			name:     "a nested image path is escaped with %2F",
			image:    "us-docker.pkg.dev/cloudrun/container/team/app:v2",
			wantAR:   true,
			wantName: "projects/cloudrun/locations/us/repositories/container/packages/team%2Fapp/tags/v2",
		},
		{
			name:     "a digest goes to dockerImages",
			image:    "us-docker.pkg.dev/p/r/img@sha256:abc123",
			wantAR:   true,
			wantName: "projects/p/locations/us/repositories/r/dockerImages/img@sha256:abc123",
		},
		{
			// ダイジェストを先に切らないと、sha256: の ":" をタグと取り違える。
			name:     "a digest wins over what looks like a tag",
			image:    "us-docker.pkg.dev/p/r/img@sha256:abc",
			wantAR:   true,
			wantName: "projects/p/locations/us/repositories/r/dockerImages/img@sha256:abc",
		},
		{name: "gcr.io is not checkable", image: "gcr.io/my-project/app:v1"},
		{name: "regional gcr.io is not checkable", image: "asia.gcr.io/my-project/app"},
		{name: "docker hub is not checkable", image: "nginx:1.27"},
		{name: "a docker hub path is not a host", image: "library/nginx"},
		{name: "an artifact registry path that is too short", image: "us-docker.pkg.dev/p/r"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseImageRef(tt.image)
			if got.IsArtifactRegistry() != tt.wantAR {
				t.Fatalf("IsArtifactRegistry() = %v, want %v (%+v)", got.IsArtifactRegistry(), tt.wantAR, got)
			}
			if !tt.wantAR {
				return
			}
			if name := got.resourceName(); name != tt.wantName {
				t.Errorf("resourceName() = %q, want %q", name, tt.wantName)
			}
		})
	}
}

// TestParseImageRefDoesNotTreatAPathAsAHost は、最初の要素にドットもコロンも無ければ
// ホストとみなさないことを確認する。取り違えると "team/app" の "team" をレジストリとして
// 扱ってしまう。
func TestParseImageRefDoesNotTreatAPathAsAHost(t *testing.T) {
	got := parseImageRef("library/nginx:1.27")
	if got.Host != "" {
		t.Errorf("Host = %q, want it to be treated as a Docker Hub path", got.Host)
	}
	if got.Tag != "1.27" {
		t.Errorf("Tag = %q, want 1.27", got.Tag)
	}
}

func TestContainerImages(t *testing.T) {
	svc := &run.Service{
		Spec: &run.ServiceSpec{
			Template: &run.RevisionTemplate{
				Spec: &run.RevisionSpec{
					Containers: []*run.Container{
						{Image: "gcr.io/p/a:v1"},
						nil,
						{Image: ""},
						{Image: "gcr.io/p/b:v1"},
						{Image: "gcr.io/p/a:v1"}, // 重複
					},
				},
			},
		},
	}
	got := containerImages(svc)
	want := []string{"gcr.io/p/a:v1", "gcr.io/p/b:v1"}
	if len(got) != len(want) {
		t.Fatalf("containerImages() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("containerImages()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestContainerImagesIsNilSafe(t *testing.T) {
	if got := containerImages(&run.Service{}); got != nil {
		t.Errorf("containerImages() = %v, want nil", got)
	}
}
