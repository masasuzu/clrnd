package cloudrun

import (
	"strings"
	"testing"

	run "google.golang.org/api/run/v1"
)

// serviceWithTraffic builds a live service with spec.traffic and status.traffic.
// The spec side is "the split currently declared" and the status side is "the split actually
// being served"; choosing where the remainder goes looks at the latter. latest is both the latest
// ready and the latest created revision, the state once a rollout has settled.
func serviceWithTraffic(spec []*run.TrafficTarget, status []*run.TrafficTarget, latest string) *run.Service {
	return &run.Service{
		ApiVersion: manifestAPIVersion,
		Kind:       manifestKind,
		Metadata:   &run.ObjectMeta{Name: "my-svc"},
		Spec: &run.ServiceSpec{
			Template: &run.RevisionTemplate{
				Spec: &run.RevisionSpec{Containers: []*run.Container{{Image: "gcr.io/p/i:v1"}}},
			},
			Traffic: spec,
		},
		Status: &run.ServiceStatus{
			LatestReadyRevisionName:   latest,
			LatestCreatedRevisionName: latest,
			Traffic:                   status,
		},
	}
}

// TestShiftTrafficTargetSplitsAgainstTheLargestShare checks that, when the percentage is below
// 100, the remainder goes to "the revision currently receiving the most". This is the canary
// shape.
func TestShiftTrafficTargetSplitsAgainstTheLargestShare(t *testing.T) {
	live := serviceWithTraffic(
		[]*run.TrafficTarget{{RevisionName: "my-svc-00007-abc", Percent: 100}},
		[]*run.TrafficTarget{
			{RevisionName: "my-svc-00007-abc", Percent: 90},
			{RevisionName: "my-svc-00006-def", Percent: 10},
		},
		"my-svc-00008-ghi")

	got, err := ShiftTrafficTarget(live, SplitRequest{Revision: "my-svc-00008-ghi", Percent: 20})
	if err != nil {
		t.Fatalf("ShiftTrafficTarget() error = %v", err)
	}
	want := []*run.TrafficTarget{
		{RevisionName: "my-svc-00008-ghi", Percent: 20},
		{RevisionName: "my-svc-00007-abc", Percent: 80},
	}
	assertTraffic(t, got.Spec.Traffic, want)
}

// TestShiftTrafficTargetToLatestFollowsTheNewest checks that --to-latest sets latestRevision
// rather than pinning a revision name. This is the path that returns traffic pinned by a rollback
// to the latest revision (the way out of the dead end).
func TestShiftTrafficTargetToLatestFollowsTheNewest(t *testing.T) {
	live := serviceWithTraffic(
		[]*run.TrafficTarget{{RevisionName: "my-svc-00006-def", Percent: 100}},
		[]*run.TrafficTarget{{RevisionName: "my-svc-00006-def", Percent: 100}},
		"my-svc-00008-ghi")

	got, err := ShiftTrafficTarget(live, SplitRequest{Latest: true, Percent: 100})
	if err != nil {
		t.Fatalf("ShiftTrafficTarget() error = %v", err)
	}
	if len(got.Spec.Traffic) != 1 {
		t.Fatalf("traffic = %+v, want a single target", got.Spec.Traffic)
	}
	target := got.Spec.Traffic[0]
	if !target.LatestRevision || target.RevisionName != "" || target.Percent != 100 {
		t.Errorf("traffic[0] = %+v, want latestRevision at 100%%", target)
	}
}

// TestShiftTrafficTargetToLatestRefusesWhenOnlyTheLatestServes checks that --to-latest with a
// --percent below 100 is refused when the latest revision is the only one serving. The remainder
// would have to stay on the latest itself, which points two entries at the same revision and is
// no split, so the error points at --percent 100 instead.
func TestShiftTrafficTargetToLatestRefusesWhenOnlyTheLatestServes(t *testing.T) {
	live := serviceWithTraffic(
		nil,
		[]*run.TrafficTarget{
			{RevisionName: "my-svc-00008-ghi", Percent: 100, LatestRevision: true},
			{RevisionName: "my-svc-00007-abc", Percent: 0},
		},
		"my-svc-00008-ghi")

	_, err := ShiftTrafficTarget(live, SplitRequest{Latest: true, Percent: 10})
	if err == nil {
		t.Fatal("ShiftTrafficTarget() error = nil, want it to refuse: nothing else is serving")
	}
	if !strings.Contains(err.Error(), "--percent 100") {
		t.Errorf("error = %v, want it to point at --percent 100", err)
	}
}

// TestShiftTrafficTargetToLatestCanariesARevisionDeployedWithoutTraffic checks the canary right
// after deploy --no-traffic: the new revision is the latest created but serves nothing, while
// latestReadyRevisionName still names the stable revision serving 100%. --to-latest resolves to
// the new revision, so the stable one keeps the rest rather than being refused as "already
// serving".
func TestShiftTrafficTargetToLatestCanariesARevisionDeployedWithoutTraffic(t *testing.T) {
	live := serviceWithTraffic(
		[]*run.TrafficTarget{{RevisionName: "my-svc-00007-abc", Percent: 100}},
		[]*run.TrafficTarget{{RevisionName: "my-svc-00007-abc", Percent: 100}},
		"my-svc-00007-abc")
	live.Status.LatestCreatedRevisionName = "my-svc-00008-ghi"

	got, err := ShiftTrafficTarget(live, SplitRequest{Latest: true, Percent: 10})
	if err != nil {
		t.Fatalf("ShiftTrafficTarget() error = %v", err)
	}
	want := []*run.TrafficTarget{
		{LatestRevision: true, Percent: 10},
		{RevisionName: "my-svc-00007-abc", Percent: 90},
	}
	assertTraffic(t, got.Spec.Traffic, want)
}

// TestShiftTrafficTargetKeepsTags checks that tagged routes are kept at 0%.
// If a tag URL disappeared just because the percentages moved, the means of checking that pointed
// at it would be lost.
func TestShiftTrafficTargetKeepsTags(t *testing.T) {
	live := serviceWithTraffic(
		[]*run.TrafficTarget{
			{RevisionName: "my-svc-00007-abc", Percent: 100},
			{RevisionName: "my-svc-00006-def", Percent: 0, Tag: "previous"},
		},
		[]*run.TrafficTarget{{RevisionName: "my-svc-00007-abc", Percent: 100}},
		"my-svc-00007-abc")

	got, err := ShiftTrafficTarget(live, SplitRequest{Revision: "my-svc-00006-def", Percent: 100})
	if err != nil {
		t.Fatalf("ShiftTrafficTarget() error = %v", err)
	}
	var tagged *run.TrafficTarget
	for _, t := range got.Spec.Traffic {
		if t.Tag == "previous" {
			tagged = t
		}
	}
	if tagged == nil {
		t.Fatalf("traffic = %+v, want the tagged entry kept", got.Spec.Traffic)
	}
	if tagged.Percent != 0 {
		t.Errorf("tagged entry = %+v, want it pinned at 0%%", tagged)
	}
}

// TestShiftTrafficTargetLeavesTheTemplateAlone checks that it does not touch the template
// (= does not create a new revision) and does not mutate its argument.
func TestShiftTrafficTargetLeavesTheTemplateAlone(t *testing.T) {
	live := serviceWithTraffic(
		[]*run.TrafficTarget{{RevisionName: "my-svc-00007-abc", Percent: 100}},
		[]*run.TrafficTarget{{RevisionName: "my-svc-00007-abc", Percent: 100}},
		"my-svc-00007-abc")

	got, err := ShiftTrafficTarget(live, SplitRequest{Revision: "my-svc-00006-def", Percent: 100})
	if err != nil {
		t.Fatalf("ShiftTrafficTarget() error = %v", err)
	}
	if got.Spec.Template != live.Spec.Template {
		t.Error("the template was replaced; traffic changes must not create a revision")
	}
	if len(live.Spec.Traffic) != 1 || live.Spec.Traffic[0].RevisionName != "my-svc-00007-abc" {
		t.Errorf("the live service was modified: %+v", live.Spec.Traffic)
	}
}

func TestValidateSplitRequest(t *testing.T) {
	tests := []struct {
		name string
		req  SplitRequest
		want string // string the error should contain. When empty, success.
	}{
		{"revision", SplitRequest{Revision: "my-svc-00007-abc", Percent: 100}, ""},
		{"latest", SplitRequest{Latest: true, Percent: 50}, ""},
		{"both", SplitRequest{Revision: "r", Latest: true, Percent: 100}, "cannot be combined"},
		{"neither", SplitRequest{Percent: 100}, "--to or --to-latest"},
		{"zero percent", SplitRequest{Latest: true}, "between 1 and 100"},
		{"over 100", SplitRequest{Latest: true, Percent: 101}, "between 1 and 100"},
		{"negative", SplitRequest{Latest: true, Percent: -1}, "between 1 and 100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSplitRequest(KindService, tt.req)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("ValidateSplitRequest() error = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("ValidateSplitRequest() error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestLargestShareIsDeterministic checks that, on a tie, the result does not change from run to
// run (if it followed map iteration order, the same input would pick a different revision).
func TestLargestShareIsDeterministic(t *testing.T) {
	status := statusWithTraffic(
		TrafficTarget{RevisionName: "my-svc-00007-abc", Percent: 50},
		TrafficTarget{RevisionName: "my-svc-00006-def", Percent: 50},
	)
	for i := 0; i < 20; i++ {
		got, share := largestShare(trafficByRevision(status))
		if got != "my-svc-00006-def" || share != 50 {
			t.Fatalf("largestShare() = %q, %d; want the name-ordered winner on every run", got, share)
		}
	}
}

// TestShiftTrafficTargetRefusesToDemoteTheServingRevision checks that a partial shift whose target
// is the revision already carrying production traffic is refused.
//
// With an implementation that hands the remainder to "the largest excluding the target", running
// --to-latest --percent 10 mid-canary (stable 90% / old 10%) drops the stable revision to 10% and
// gives the old one 90%. Nobody wants that split, so it refuses instead of promoting the
// runner-up.
func TestShiftTrafficTargetRefusesToDemoteTheServingRevision(t *testing.T) {
	live := serviceWithTraffic(
		nil,
		[]*run.TrafficTarget{
			{RevisionName: "my-svc-00007-abc", Percent: 90},
			{RevisionName: "my-svc-00006-def", Percent: 10},
		},
		"my-svc-00007-abc")

	for _, req := range []SplitRequest{
		{Latest: true, Percent: 10},                 // latestRevision resolves to the stable revision
		{Revision: "my-svc-00007-abc", Percent: 10}, // the same one pointed at by name
	} {
		_, err := ShiftTrafficTarget(live, req)
		if err == nil {
			t.Fatalf("ShiftTrafficTarget(%+v) error = nil, want it to refuse", req)
		}
		if !strings.Contains(err.Error(), "already serving") {
			t.Errorf("ShiftTrafficTarget(%+v) error = %v, want it to say the target already serves", req, err)
		}
	}
}

// TestShiftTrafficTargetSplitsAgainstTheStableRevisionMidCanary checks that, even mid-canary,
// targeting a different revision puts the remainder on the stable revision.
func TestShiftTrafficTargetSplitsAgainstTheStableRevisionMidCanary(t *testing.T) {
	live := serviceWithTraffic(
		nil,
		[]*run.TrafficTarget{
			{RevisionName: "my-svc-00007-abc", Percent: 90},
			{RevisionName: "my-svc-00006-def", Percent: 10},
		},
		"my-svc-00007-abc")

	got, err := ShiftTrafficTarget(live, SplitRequest{Revision: "my-svc-00006-def", Percent: 30})
	if err != nil {
		t.Fatalf("ShiftTrafficTarget() error = %v", err)
	}
	assertTraffic(t, got.Spec.Traffic, []*run.TrafficTarget{
		{RevisionName: "my-svc-00006-def", Percent: 30},
		{RevisionName: "my-svc-00007-abc", Percent: 70},
	})
}

// TestPinnedTrafficFixesTheLatestPointer checks that the pinning deploy --no-traffic uses replaces
// latestRevision with a concrete revision name. If this does not become a name, the revision about
// to be created receives all of the traffic.
func TestPinnedTrafficFixesTheLatestPointer(t *testing.T) {
	live := serviceWithTraffic(
		nil,
		[]*run.TrafficTarget{
			{RevisionName: "my-svc-00007-abc", Percent: 100, LatestRevision: true},
			{RevisionName: "my-svc-00006-def", Percent: 0, Tag: "previous", Url: "https://example.test"},
		},
		"my-svc-00007-abc")

	got := pinnedTraffic(live)
	want := []*run.TrafficTarget{
		{RevisionName: "my-svc-00007-abc", Percent: 100},
		{RevisionName: "my-svc-00006-def", Percent: 0, Tag: "previous"},
	}
	assertTraffic(t, got, want)
	for _, target := range got {
		if target.LatestRevision {
			t.Errorf("target %+v still follows the latest revision", target)
		}
	}
}

// TestCheckLatestCanServe checks that --to-latest is refused only when the newest created revision
// failed: one that has not served yet (Ready=True, Retired) or is still rolling out (Unknown)
// cannot be told apart from a working one, and an older failed revision is not the target.
func TestCheckLatestCanServe(t *testing.T) {
	revisions := func(newest Revision) Revisions {
		return Revisions{newest, {Name: "my-pool-00002-bbb", Ready: conditionFalse, Reason: "ContainerMissing"},
			{Name: "my-pool-00001-aaa", Ready: conditionTrue, Percent: 100}}
	}
	err := checkLatestCanServe(revisions(Revision{Name: "my-pool-00003-ccc", Ready: conditionFalse, Reason: "ContainerMissing"}),
		"my-pool-00003-ccc", workerPoolSplitWords)
	if err == nil || !strings.Contains(err.Error(),
		`the latest revision "my-pool-00003-ccc" failed (ContainerMissing), and --to-latest would run the instances on it`) {
		t.Errorf("checkLatestCanServe(failed) error = %v", err)
	}
	for _, newest := range []Revision{
		{Name: "my-pool-00003-ccc", Ready: conditionTrue, Reason: "Retired"},
		{Name: "my-pool-00003-ccc", Ready: "Unknown"},
	} {
		if err := checkLatestCanServe(revisions(newest), "my-pool-00003-ccc", workerPoolSplitWords); err != nil {
			t.Errorf("checkLatestCanServe(%+v) error = %v, want nil", newest, err)
		}
	}
}

// TestValidateSplitRequestNamesWhatTheKindSplits checks that the message for a missing target puts
// it in the kind's terms, and that a kind without revisions has no split to change.
func TestValidateSplitRequestNamesWhatTheKindSplits(t *testing.T) {
	for kind, want := range map[Kind]string{
		KindService:    "no revision to send traffic to",
		KindWorkerPool: "no revision to run the instances on",
		KindJob:        "a job has no revisions",
	} {
		if err := ValidateSplitRequest(kind, SplitRequest{Percent: 100}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateSplitRequest(%s) error = %v, want it to mention %q", kind.Name(), err, want)
		}
	}
}

func TestHasSplit(t *testing.T) {
	tests := []struct {
		name     string
		kind     Kind
		manifest string
		want     bool
	}{
		{"service with traffic", KindService, validManifest + "  traffic:\n  - revisionName: my-svc-00007-abc\n    percent: 100\n", true},
		{"service without", KindService, validManifest, false},
		{"worker pool with instanceSplits", KindWorkerPool, validWorkerPoolManifest + "  instanceSplits:\n  - latestRevision: true\n    percent: 100\n", true},
		{"worker pool without", KindWorkerPool, validWorkerPoolManifest, false},
		{"job", KindJob, validJobManifest, false},
	}
	for _, tt := range tests {
		if got, err := HasSplit(tt.kind, []byte(tt.manifest)); err != nil || got != tt.want {
			t.Errorf("HasSplit(%s) = %v, %v; want %v, nil", tt.name, got, err, tt.want)
		}
	}
}

// assertTraffic checks that the sequence of revisionName / tag / percent matches.
func assertTraffic(t *testing.T, got, want []*run.TrafficTarget) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("traffic = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].RevisionName != want[i].RevisionName ||
			got[i].Tag != want[i].Tag ||
			got[i].LatestRevision != want[i].LatestRevision ||
			got[i].Percent != want[i].Percent {
			t.Errorf("traffic[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
