package cloudrun

import (
	"strings"
	"testing"
	"time"

	run "google.golang.org/api/run/v1"
)

func TestRefreshSuffix(t *testing.T) {
	now := time.Date(2026, 8, 23, 4, 5, 6, 0, time.FixedZone("JST", 9*60*60))
	// In UTC this is 2026-08-22 19:05:06.
	if got, want := RefreshSuffix(now), "r260822190506"; got != want {
		t.Errorf("RefreshSuffix() = %q, want %q", got, want)
	}
	// It includes the seconds, so names do not collide even on back-to-back runs.
	a := RefreshSuffix(time.Unix(1000, 0))
	b := RefreshSuffix(time.Unix(1001, 0))
	if a == b {
		t.Errorf("RefreshSuffix() = %q for both seconds, want distinct names", a)
	}
}

// liveForRefresh builds a live service for refresh to act on.
func liveForRefresh(templateName string) *run.Service {
	meta := &run.ObjectMeta{Annotations: map[string]string{"run.googleapis.com/client-name": "gcloud"}}
	if templateName != "" {
		meta.Name = templateName
	}
	return &run.Service{
		Metadata: &run.ObjectMeta{Name: "my-svc"},
		Spec: &run.ServiceSpec{
			Template: &run.RevisionTemplate{
				Metadata: meta,
				Spec:     &run.RevisionSpec{Containers: []*run.Container{{Image: "img"}}},
			},
		},
	}
}

func TestRefreshTarget(t *testing.T) {
	live := liveForRefresh("")

	got, err := RefreshTarget(live, "my-svc", "r260822190506")
	if err != nil {
		t.Fatalf("RefreshTarget() error = %v", err)
	}
	if name := revisionName(got); name != "my-svc-r260822190506" {
		t.Errorf("revision name = %q, want the service name prefix plus the suffix", name)
	}
	// Template metadata other than the name is kept.
	if got.Spec.Template.Metadata.Annotations["run.googleapis.com/client-name"] != "gcloud" {
		t.Error("RefreshTarget dropped the other template annotations")
	}
	// The argument is not mutated.
	if revisionName(live) != "" {
		t.Errorf("RefreshTarget mutated its argument: %q", revisionName(live))
	}
}

func TestRefreshTargetReplacesAnExistingName(t *testing.T) {
	// The name set by the previous refresh is replaced with a new one. With the same name, no
	// new revision is created (it becomes a 409).
	live := liveForRefresh("my-svc-r260101000000")

	got, err := RefreshTarget(live, "my-svc", "r260822190506")
	if err != nil {
		t.Fatalf("RefreshTarget() error = %v", err)
	}
	if name := revisionName(got); name != "my-svc-r260822190506" {
		t.Errorf("revision name = %q, want the new suffix", name)
	}
	if revisionName(live) != "my-svc-r260101000000" {
		t.Error("RefreshTarget mutated its argument")
	}
}

func TestRefreshTargetErrors(t *testing.T) {
	tests := []struct {
		name    string
		live    *run.Service
		service string
		suffix  string
		wantErr string
	}{
		{name: "nil service", live: nil, service: "my-svc", suffix: "r1", wantErr: "no spec.template to refresh"},
		{
			name:    "no template",
			live:    &run.Service{Spec: &run.ServiceSpec{}},
			service: "my-svc", suffix: "r1",
			wantErr: "no spec.template to refresh",
		},
		{
			name: "no suffix", live: liveForRefresh(""), service: "my-svc",
			wantErr: "no revision suffix",
		},
		{
			// Cloud Run only accepts fewer than 64 characters. Say so up front in words the
			// user can act on.
			name: "too long", live: liveForRefresh(""),
			service: strings.Repeat("a", 50), suffix: "r260822190506",
			wantErr: "Cloud Run allows at most 63",
		},
		{
			name: "invalid characters", live: liveForRefresh(""),
			service: "my-svc", suffix: "R_260822",
			wantErr: "not valid",
		},
		{
			name: "ends with a hyphen", live: liveForRefresh(""),
			service: "my-svc", suffix: "r-",
			wantErr: "not valid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := RefreshTarget(tt.live, tt.service, tt.suffix)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("RefreshTarget() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// withTraffic sets spec.traffic on a live service.
func withTraffic(live *run.Service, targets ...*run.TrafficTarget) *run.Service {
	live.Spec.Traffic = targets
	return live
}

// TestRefreshTargetRejectsPinnedTraffic checks that it refuses when traffic is pinned to a
// specific revision. This is the state right after a rollback, and refreshing as is creates a
// new revision that stays at 0%.
func TestRefreshTargetRejectsPinnedTraffic(t *testing.T) {
	live := withTraffic(liveForRefresh(""),
		&run.TrafficTarget{RevisionName: "my-svc-00006-def", Percent: 100})

	_, err := RefreshTarget(live, "my-svc", "r260822190506")
	if err == nil {
		t.Fatal("RefreshTarget() error = nil, want it to refuse pinned traffic")
	}
	if !strings.Contains(err.Error(), "receives no traffic") {
		t.Errorf("RefreshTarget() error = %v, want it to explain that nothing would be served", err)
	}
}

// TestRefreshTargetAllowsTrafficThatFollowsTheLatest checks that it goes through when there is an
// entry pointing at the latest revision (even alongside a tagged route).
func TestRefreshTargetAllowsTrafficThatFollowsTheLatest(t *testing.T) {
	live := withTraffic(liveForRefresh(""),
		&run.TrafficTarget{LatestRevision: true, Percent: 90},
		&run.TrafficTarget{RevisionName: "my-svc-00006-def", Percent: 10, Tag: "previous"})

	if _, err := RefreshTarget(live, "my-svc", "r260822190506"); err != nil {
		t.Fatalf("RefreshTarget() error = %v, want a canary against the latest revision to be allowed", err)
	}
}

// TestRefreshTargetRejectsATagOnlyLatestEntry checks that a tag-only entry with a 0 share is not
// counted as serving, even when it is latestRevision.
func TestRefreshTargetRejectsATagOnlyLatestEntry(t *testing.T) {
	live := withTraffic(liveForRefresh(""),
		&run.TrafficTarget{RevisionName: "my-svc-00006-def", Percent: 100},
		&run.TrafficTarget{LatestRevision: true, Percent: 0, Tag: "next"})

	if _, err := RefreshTarget(live, "my-svc", "r260822190506"); err == nil {
		t.Fatal("RefreshTarget() error = nil, want a 0% tag entry not to count as serving")
	}
}

// TestRefreshTargetRejectsTheSameRevisionName checks that it refuses when the generated name is
// the same as the current one. With the same name no new revision is created, the diff is empty,
// and it ends in "No changes.", succeeding without anything having happened.
func TestRefreshTargetRejectsTheSameRevisionName(t *testing.T) {
	live := liveForRefresh("my-svc-r260822190506")

	_, err := RefreshTarget(live, "my-svc", "r260822190506")
	if err == nil {
		t.Fatal("RefreshTarget() error = nil, want it to refuse a name that would not create a revision")
	}
	if !strings.Contains(err.Error(), "already the current template revision") {
		t.Errorf("RefreshTarget() error = %v", err)
	}
}

// TestRefreshTargetAcceptsTheLongestAllowedName checks the boundary.
func TestRefreshTargetAcceptsTheLongestAllowedName(t *testing.T) {
	// "<49 characters>-r260822190506" = 49 + 1 + 13 = exactly 63 characters.
	service := strings.Repeat("a", 49)
	if _, err := RefreshTarget(liveForRefresh(""), service, "r260822190506"); err != nil {
		t.Fatalf("RefreshTarget() error = %v, want 63 characters to be accepted", err)
	}
}
