package cloudrun

import (
	"strings"
	"testing"

	run "google.golang.org/api/run/v1"
)

// rev builds a Revision for tests (in the shape ListRevisions returns).
func rev(name, ready string, percent int64) Revision {
	return Revision{Name: name, Ready: ready, Percent: percent}
}

func TestSelectRollbackRevision(t *testing.T) {
	// Newest first. 00007 is currently receiving traffic.
	revisions := Revisions{
		rev("my-svc-00008-ghi", conditionFalse, 0),
		rev("my-svc-00007-abc", conditionTrue, 100),
		rev("my-svc-00006-def", conditionTrue, 0),
		rev("my-svc-00005-jkl", conditionTrue, 0),
	}

	tests := []struct {
		name      string
		revisions Revisions
		requested string
		want      string
		wantErr   string
	}{
		{
			name:      "defaults to the one before the serving revision",
			revisions: revisions,
			want:      "my-svc-00006-def",
		},
		{
			name:      "explicit revision is used as is",
			revisions: revisions,
			requested: "my-svc-00005-jkl",
			want:      "my-svc-00005-jkl",
		},
		{
			// An explicitly requested revision is chosen even when it is not Ready (the
			// warning is cmd's job).
			name:      "explicit revision may be not ready",
			revisions: revisions,
			requested: "my-svc-00008-ghi",
			want:      "my-svc-00008-ghi",
		},
		{
			name:      "unknown revision is rejected",
			revisions: revisions,
			requested: "other-svc-00001-xyz",
			wantErr:   `revision "other-svc-00001-xyz" does not belong to this service`,
		},
		{
			name: "skips revisions that are not ready",
			revisions: Revisions{
				rev("my-svc-00004", conditionTrue, 100),
				rev("my-svc-00003", conditionFalse, 0),
				rev("my-svc-00002", conditionTrue, 0),
			},
			want: "my-svc-00002",
		},
		{
			name:      "no traffic anywhere",
			revisions: Revisions{rev("my-svc-00001", conditionTrue, 0)},
			wantErr:   "no revision is currently receiving traffic",
		},
		{
			name:      "nothing older to roll back to",
			revisions: Revisions{rev("my-svc-00001", conditionTrue, 100)},
			wantErr:   `no ready revision older than "my-svc-00001"`,
		},
		{
			name: "older revisions are all broken",
			revisions: Revisions{
				rev("my-svc-00003", conditionTrue, 100),
				rev("my-svc-00002", conditionFalse, 0),
			},
			wantErr: `no ready revision older than "my-svc-00003"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SelectRollbackRevision(tt.revisions, tt.requested)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("SelectRollbackRevision() error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SelectRollbackRevision() error = %v", err)
			}
			if got.Name != tt.want {
				t.Errorf("SelectRollbackRevision() = %q, want %q", got.Name, tt.want)
			}
		})
	}
}

// TestSelectRollbackRevisionDuringACanary checks that, while traffic is split, it rolls back to
// "the stable revision". Taking the one with the largest share as the current revision mistakes
// the stable revision for the current one, rolls back to the one before it, and skips past the
// known-good version.
func TestSelectRollbackRevisionDuringACanary(t *testing.T) {
	revisions := Revisions{
		rev("my-svc-00003", conditionTrue, 10), // the new canary
		rev("my-svc-00002", conditionTrue, 90), // the stable revision
		rev("my-svc-00001", conditionTrue, 0),  // two generations back
	}
	got, err := SelectRollbackRevision(revisions, "")
	if err != nil {
		t.Fatalf("SelectRollbackRevision() error = %v", err)
	}
	if got.Name != "my-svc-00002" {
		t.Errorf("SelectRollbackRevision() = %q, want the stable revision the canary is being tested against",
			got.Name)
	}
}

// TestSelectRollbackRevisionUsesTheNewestServingRevision checks that, even when an older revision
// has a larger share, the current revision is "the newest of those receiving traffic".
func TestSelectRollbackRevisionUsesTheNewestServingRevision(t *testing.T) {
	revisions := Revisions{
		rev("my-svc-00004", conditionTrue, 0),  // not serving
		rev("my-svc-00003", conditionTrue, 1),  // the newest serving revision
		rev("my-svc-00002", conditionTrue, 99), // the largest share, but older
		rev("my-svc-00001", conditionTrue, 0),
	}
	got, err := SelectRollbackRevision(revisions, "")
	if err != nil {
		t.Fatalf("SelectRollbackRevision() error = %v", err)
	}
	if got.Name != "my-svc-00002" {
		t.Errorf("SelectRollbackRevision() = %q, want the one just below the newest serving revision",
			got.Name)
	}
}

func TestRollbackTarget(t *testing.T) {
	live := &run.Service{
		Metadata: &run.ObjectMeta{Name: "my-svc"},
		Spec: &run.ServiceSpec{
			Template: &run.RevisionTemplate{
				Metadata: &run.ObjectMeta{Name: "my-svc-00007-abc"},
				Spec:     &run.RevisionSpec{Containers: []*run.Container{{Image: "img"}}},
			},
			Traffic: []*run.TrafficTarget{
				{LatestRevision: true, Percent: 100},
				{RevisionName: "my-svc-00006-def", Percent: 0, Tag: "previous"},
			},
		},
	}

	got, err := RollbackTarget(live, "my-svc-00006-def")
	if err != nil {
		t.Fatalf("RollbackTarget() error = %v", err)
	}

	if len(got.Spec.Traffic) != 2 {
		t.Fatalf("Traffic = %+v, want the rollback target plus the kept tag", got.Spec.Traffic)
	}
	if got.Spec.Traffic[0].RevisionName != "my-svc-00006-def" || got.Spec.Traffic[0].Percent != 100 {
		t.Errorf("Traffic[0] = %+v, want 100%% to the target", got.Spec.Traffic[0])
	}
	if got.Spec.Traffic[0].LatestRevision {
		t.Error("Traffic[0].LatestRevision = true, want the traffic pinned to a revision")
	}
	// Tagged routes are kept (with a 0 share).
	if got.Spec.Traffic[1].Tag != "previous" || got.Spec.Traffic[1].Percent != 0 {
		t.Errorf("Traffic[1] = %+v, want the tag kept at 0%%", got.Spec.Traffic[1])
	}
	// The template is not touched (so no new revision is created).
	if got.Spec.Template != live.Spec.Template {
		t.Error("RollbackTarget must not touch spec.template")
	}
	// The argument is not mutated.
	if len(live.Spec.Traffic) != 2 || !live.Spec.Traffic[0].LatestRevision {
		t.Errorf("RollbackTarget mutated its argument: %+v", live.Spec.Traffic)
	}
}

func TestRollbackTargetErrors(t *testing.T) {
	tests := []struct {
		name     string
		live     *run.Service
		revision string
		wantErr  string
	}{
		{name: "nil service", live: nil, revision: "r", wantErr: "no spec to roll back"},
		{name: "no spec", live: &run.Service{}, revision: "r", wantErr: "no spec to roll back"},
		{
			name:     "no revision",
			live:     &run.Service{Spec: &run.ServiceSpec{}},
			wantErr:  "no revision to roll back to",
			revision: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := RollbackTarget(tt.live, tt.revision); err == nil ||
				!strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("RollbackTarget() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
