package cloudrun

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	run "google.golang.org/api/run/v1"
)

// revision builds a revision for tests. When ready is empty, it has no Ready condition.
func revision(name, image, created, ready, reason string) *run.Revision {
	r := &run.Revision{
		Metadata: &run.ObjectMeta{Name: name, CreationTimestamp: created},
		Spec:     &run.RevisionSpec{Containers: []*run.Container{{Image: image}}},
		Status:   &run.RevisionStatus{},
	}
	if ready != "" {
		r.Status.Conditions = []*run.GoogleCloudRunV1Condition{
			{Type: conditionReady, Status: ready, Reason: reason},
		}
	}
	return r
}

// statusWithTraffic builds a Status that holds only status.traffic.
func statusWithTraffic(targets ...TrafficTarget) *Status {
	return &Status{Traffic: targets}
}

func TestNewRevisions(t *testing.T) {
	items := []*run.Revision{
		revision("my-svc-00006-def", "gcr.io/p/i:v1", "2026-08-21T09:00:00Z", conditionTrue, ""),
		revision("my-svc-00007-abc", "gcr.io/p/i:v2", "2026-08-22T10:00:00Z", conditionTrue, ""),
	}
	status := statusWithTraffic(
		TrafficTarget{RevisionName: "my-svc-00007-abc", Percent: 90},
		TrafficTarget{RevisionName: "my-svc-00006-def", Percent: 10},
	)

	got := newRevisions(items, status, nil)
	if len(got) != 2 {
		t.Fatalf("newRevisions() = %+v, want 2 entries", got)
	}
	// Newest first.
	if got[0].Name != "my-svc-00007-abc" || got[1].Name != "my-svc-00006-def" {
		t.Errorf("order = %q, %q, want newest first", got[0].Name, got[1].Name)
	}
	if strings.Join(got[0].Images, ",") != "gcr.io/p/i:v2" || got[0].Percent != 90 || got[0].Ready != conditionTrue {
		t.Errorf("newRevisions()[0] = %+v", got[0])
	}
	if got[1].Percent != 10 {
		t.Errorf("newRevisions()[1].Percent = %d, want 10", got[1].Percent)
	}
}

func TestNewRevisionsMergesTrafficEntries(t *testing.T) {
	// The same revision can appear in several entries, for a "percentage" and for a "tag".
	items := []*run.Revision{revision("my-svc-00007-abc", "img", "2026-08-22T10:00:00Z", conditionTrue, "")}
	status := statusWithTraffic(
		TrafficTarget{RevisionName: "my-svc-00007-abc", Percent: 60},
		TrafficTarget{RevisionName: "my-svc-00007-abc", Percent: 40, Tag: "canary"},
		TrafficTarget{RevisionName: "my-svc-00007-abc", Percent: 0, Tag: "previous"},
	)

	got := newRevisions(items, status, nil)
	if got[0].Percent != 100 {
		t.Errorf("Percent = %d, want 100 (entries must be summed)", got[0].Percent)
	}
	if strings.Join(got[0].Tags, ",") != "canary,previous" {
		t.Errorf("Tags = %v, want both tags", got[0].Tags)
	}
}

// TestNewRevisionsKeepsEveryContainerImage checks that every image of a revision with sidecars
// shows up. Back when only the first one was returned, images that were not displayed were
// actually running.
func TestNewRevisionsKeepsEveryContainerImage(t *testing.T) {
	item := revision("my-svc-00007-abc", "gcr.io/p/app:v2", "2026-08-22T10:00:00Z", conditionTrue, "")
	item.Spec.Containers = append(item.Spec.Containers,
		&run.Container{Image: ""}, // a container with no image is skipped
		&run.Container{Image: "gcr.io/p/proxy:v1"})

	got := newRevisions([]*run.Revision{item}, nil, nil)
	want := []string{"gcr.io/p/app:v2", "gcr.io/p/proxy:v1"}
	if strings.Join(got[0].Images, ",") != strings.Join(want, ",") {
		t.Errorf("Images = %v, want %v in spec order", got[0].Images, want)
	}
	// So as not to break existing JSON consumers (jq '.[].image'), image is kept and points at
	// the first one.
	if got[0].Image != want[0] {
		t.Errorf("Image = %q, want the first container %q", got[0].Image, want[0])
	}
	// The table's IMAGE column shows them all too.
	text := Revisions{got[0]}.Text()
	if !strings.Contains(text, "gcr.io/p/app:v2,gcr.io/p/proxy:v1") {
		t.Errorf("Text() = %q, want both images in the IMAGE column", text)
	}
}

func TestNewRevisionsIsNilSafe(t *testing.T) {
	items := []*run.Revision{
		nil,
		{},                                     // neither metadata nor spec
		{Metadata: &run.ObjectMeta{Name: "x"}}, // no spec
	}
	got := newRevisions(items, nil, nil)
	if len(got) != 2 {
		t.Fatalf("newRevisions() = %+v, want the nil entry skipped", got)
	}
	for _, r := range got {
		if r.Image != "" || len(r.Images) != 0 || r.Ready != "" {
			t.Errorf("revision = %+v, want empty fields", r)
		}
	}
}

func TestSortRevisionsFallsBackToTheName(t *testing.T) {
	// When the creation time cannot be read, it sorts by name, descending (Cloud Run numbers
	// revisions sequentially).
	items := []*run.Revision{
		revision("my-svc-00006-def", "img", "", conditionTrue, ""),
		revision("my-svc-00008-ghi", "img", "", conditionTrue, ""),
		revision("my-svc-00007-abc", "img", "", conditionTrue, ""),
	}
	got := newRevisions(items, nil, nil)
	want := []string{"my-svc-00008-ghi", "my-svc-00007-abc", "my-svc-00006-def"}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("order[%d] = %q, want %q", i, got[i].Name, name)
		}
	}
}

// TestSortRevisionsHandlesFractionalSeconds checks that sorting works on the format the real API
// returns. Cloud Run's creationTimestamp includes fractional seconds, as in
// "2026-08-22T17:51:38.143198Z".
func TestSortRevisionsHandlesFractionalSeconds(t *testing.T) {
	items := []*run.Revision{
		revision("older", "img", "2026-08-22T17:51:38.143198Z", conditionTrue, ""),
		revision("newer", "img", "2026-08-22T17:51:38.999999Z", conditionTrue, ""),
	}
	got := newRevisions(items, nil, nil)
	if got[0].Name != "newer" || got[1].Name != "older" {
		t.Errorf("order = %q, %q, want newest first", got[0].Name, got[1].Name)
	}
}

func TestSortRevisionsPutsParsableTimestampsFirst(t *testing.T) {
	items := []*run.Revision{
		revision("no-timestamp", "img", "", conditionTrue, ""),
		revision("with-timestamp", "img", "2026-08-22T10:00:00Z", conditionTrue, ""),
	}
	got := newRevisions(items, nil, nil)
	if got[0].Name != "with-timestamp" {
		t.Errorf("order = %q first, want the revision with a timestamp", got[0].Name)
	}
}

func TestRevisionsText(t *testing.T) {
	items := []*run.Revision{
		revision("my-svc-00007-abc", "gcr.io/p/i:v2", "2026-08-22T10:00:00Z", conditionTrue, ""),
		revision("my-svc-00006-def", "gcr.io/p/i:v1", "2026-08-21T09:00:00Z", conditionFalse, "RevisionFailed"),
	}
	status := statusWithTraffic(
		TrafficTarget{RevisionName: "my-svc-00007-abc", Percent: 100, Tag: "live"},
	)

	want := `REVISION          READY                   TRAFFIC  TAGS  CREATED               IMAGE
my-svc-00007-abc  True                    100%     live  2026-08-22T10:00:00Z  gcr.io/p/i:v2
my-svc-00006-def  False (RevisionFailed)  0%       -     2026-08-21T09:00:00Z  gcr.io/p/i:v1
`
	if got := newRevisions(items, status, nil).Text(); got != want {
		t.Errorf("Text() mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRevisionsTextIsEmptyWhenThereAreNone(t *testing.T) {
	if got := (Revisions{}).Text(); got != "" {
		t.Errorf("Text() = %q, want empty", got)
	}
}

func TestClientListRevisions(t *testing.T) {
	var paths []string
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/revisions") {
			return http.StatusOK, &run.ListRevisionsResponse{Items: []*run.Revision{
				revision("my-svc-00007-abc", "gcr.io/p/i:v2", "2026-08-22T10:00:00Z", conditionTrue, ""),
			}}
		}
		return http.StatusOK, readyService()
	})

	got, err := c.ListRevisions(context.Background(), "my-svc")
	if err != nil {
		t.Fatalf("ListRevisions() error = %v", err)
	}
	if len(got) != 1 || got[0].Name != "my-svc-00007-abc" {
		t.Fatalf("ListRevisions() = %+v", got)
	}
	// The live traffic has been joined in.
	if got[0].Percent != 100 {
		t.Errorf("Percent = %d, want 100 from the service traffic", got[0].Percent)
	}
	// It fetches both the service and the revisions.
	wantPaths := []string{
		"/apis/serving.knative.dev/v1/namespaces/test-project/services/my-svc",
		"/apis/serving.knative.dev/v1/namespaces/test-project/revisions",
	}
	if len(paths) != 2 || paths[0] != wantPaths[0] || paths[1] != wantPaths[1] {
		t.Errorf("paths = %v, want %v", paths, wantPaths)
	}
}

func TestClientListRevisionsFollowsPagination(t *testing.T) {
	var continues []string
	page := 0
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if !strings.HasSuffix(r.URL.Path, "/revisions") {
			return http.StatusOK, readyService()
		}
		continues = append(continues, r.URL.Query().Get("continue"))
		page++
		if page == 1 {
			return http.StatusOK, &run.ListRevisionsResponse{
				Items:    []*run.Revision{revision("my-svc-00007-abc", "img", "2026-08-22T10:00:00Z", conditionTrue, "")},
				Metadata: &run.ListMeta{Continue: "next-page"},
			}
		}
		return http.StatusOK, &run.ListRevisionsResponse{
			Items: []*run.Revision{revision("my-svc-00006-def", "img", "2026-08-21T09:00:00Z", conditionTrue, "")},
		}
	})

	got, err := c.ListRevisions(context.Background(), "my-svc")
	if err != nil {
		t.Fatalf("ListRevisions() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListRevisions() = %+v, want both pages", got)
	}
	if len(continues) != 2 || continues[0] != "" || continues[1] != "next-page" {
		t.Errorf("continue tokens = %v, want the second request to carry the token", continues)
	}
}

func TestClientListRevisionsPropagatesErrors(t *testing.T) {
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if strings.HasSuffix(r.URL.Path, "/revisions") {
			return http.StatusForbidden, googleAPIError(403, "denied")
		}
		return http.StatusOK, readyService()
	})

	_, err := c.ListRevisions(context.Background(), "my-svc")
	if err == nil || !strings.Contains(err.Error(), `failed to list revisions of service "my-svc"`) {
		t.Fatalf("ListRevisions() error = %v, want it to name the service", err)
	}
}

// TestClientListRevisionsFailsOnARepeatedToken checks that, when the server keeps returning the
// same Continue token, the list read so far is not returned as a success but becomes an error.
// Following responses that do not advance grows items without bound, so cutting it off is
// necessary, but a cut-off list can hold duplicates as well as gaps. If rollback treated it as a
// complete list it could roll back to the wrong version, so it is not silently truncated.
func TestClientListRevisionsFailsOnARepeatedToken(t *testing.T) {
	pages := 0
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if !strings.HasSuffix(r.URL.Path, "/revisions") {
			return http.StatusOK, readyService()
		}
		pages++
		// Always returns the same token (paging that does not advance).
		return http.StatusOK, &run.ListRevisionsResponse{
			Items:    []*run.Revision{revision("my-svc-00007-abc", "img", "2026-08-22T10:00:00Z", conditionTrue, "")},
			Metadata: &run.ListMeta{Continue: "stuck"},
		}
	})

	done := make(chan struct{})
	var got Revisions
	var err error
	go func() {
		got, err = c.ListRevisions(context.Background(), "my-svc")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ListRevisions() did not return; the pagination loop does not terminate")
	}
	if err == nil {
		t.Fatalf("ListRevisions() = %+v, want an error instead of a truncated list", got)
	}
	if !strings.Contains(err.Error(), "pagination did not advance") {
		t.Errorf("ListRevisions() error = %v, want it to name the stuck pagination", err)
	}
	if got != nil {
		t.Errorf("ListRevisions() = %+v, want no partial list alongside the error", got)
	}
	// It stops after the first page and the second page, which brought back the same token.
	if pages != 2 {
		t.Errorf("requested %d pages, want it to stop at 2", pages)
	}
}

// TestClientListRevisionsFailsAtThePageLimit checks that, when a token is still left after
// reaching the page limit, what was read so far is not returned as a success.
func TestClientListRevisionsFailsAtThePageLimit(t *testing.T) {
	pages := 0
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if !strings.HasSuffix(r.URL.Path, "/revisions") {
			return http.StatusOK, readyService()
		}
		pages++
		// Returns a different token every time (paging that advances but never ends).
		return http.StatusOK, &run.ListRevisionsResponse{
			Items:    []*run.Revision{revision(fmt.Sprintf("my-svc-%05d-abc", pages), "img", "2026-08-22T10:00:00Z", conditionTrue, "")},
			Metadata: &run.ListMeta{Continue: fmt.Sprintf("page-%d", pages)},
		}
	})

	got, err := c.ListRevisions(context.Background(), "my-svc")
	if err == nil {
		t.Fatalf("ListRevisions() returned %d revisions, want an error at the page limit", len(got))
	}
	if !strings.Contains(err.Error(), "gave up after") {
		t.Errorf("ListRevisions() error = %v, want it to name the page limit", err)
	}
	if got != nil {
		t.Errorf("ListRevisions() returned %d revisions, want no partial list alongside the error", len(got))
	}
	if pages != listRevisionsMaxPages {
		t.Errorf("requested %d pages, want it to stop at the limit of %d", pages, listRevisionsMaxPages)
	}
}

// TestSelectPrunableRevisions checks how the revisions to prune are chosen. Not deleting what must
// not be deleted is this function's requirement.
func TestSelectPrunableRevisions(t *testing.T) {
	// Newest first. 00005 is serving, and 00003 has a tag.
	all := Revisions{
		{Name: "my-svc-00007-abc"},
		{Name: "my-svc-00006-def"},
		{Name: "my-svc-00005-ghi", Percent: 100},
		{Name: "my-svc-00004-jkl"},
		{Name: "my-svc-00003-mno", Tags: []string{"previous"}},
		{Name: "my-svc-00002-pqr"},
	}

	tests := []struct {
		name string
		keep int
		want []string
	}{
		{
			name: "keeps the newest and skips protected ones",
			keep: 2,
			want: []string{"my-svc-00004-jkl", "my-svc-00002-pqr"},
		},
		{
			name: "keep 0 still protects traffic and tags",
			keep: 0,
			want: []string{"my-svc-00007-abc", "my-svc-00006-def", "my-svc-00004-jkl", "my-svc-00002-pqr"},
		},
		{
			name: "keeping everything prunes nothing",
			keep: len(all),
			want: nil,
		},
		{
			name: "a negative keep is treated as zero, not as a wildcard",
			keep: -1,
			want: []string{"my-svc-00007-abc", "my-svc-00006-def", "my-svc-00004-jkl", "my-svc-00002-pqr"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, r := range SelectPrunableRevisions(all, tt.keep) {
				got = append(got, r.Name)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("SelectPrunableRevisions(keep=%d) = %v, want %v", tt.keep, got, tt.want)
			}
		})
	}
}

// TestSelectPrunableRevisionsCountsProtectedTowardKeep checks that protected revisions also count
// toward keep. keep is how far back from the newest to look, so a protected revision inside that
// window must not push the window further back and expose an older revision to deletion.
func TestSelectPrunableRevisionsCountsProtectedTowardKeep(t *testing.T) {
	all := Revisions{
		{Name: "my-svc-00003-abc", Percent: 100},
		{Name: "my-svc-00002-def"},
		{Name: "my-svc-00001-ghi"},
	}
	got := SelectPrunableRevisions(all, 2)
	if len(got) != 1 || got[0].Name != "my-svc-00001-ghi" {
		t.Errorf("SelectPrunableRevisions(keep=2) = %+v, want only the oldest", got)
	}
}

// TestClientDeleteRevisionUsesTheNamespacedName checks that the delete is called with the
// revision's resource name built from it.
func TestClientDeleteRevisionUsesTheNamespacedName(t *testing.T) {
	c, api := newTestClient(t, func(*http.Request) (int, interface{}) {
		return http.StatusOK, &run.Status{}
	})

	if err := c.DeleteRevision(context.Background(), "my-svc-00002-pqr"); err != nil {
		t.Fatalf("DeleteRevision() error = %v", err)
	}
	recorded := api.recorded()
	if len(recorded) != 1 {
		t.Fatalf("requests = %d, want 1", len(recorded))
	}
	if recorded[0].Method != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", recorded[0].Method)
	}
	want := "/apis/serving.knative.dev/v1/namespaces/test-project/revisions/my-svc-00002-pqr"
	if recorded[0].Path != want {
		t.Errorf("path = %q, want %q", recorded[0].Path, want)
	}
}

// TestClientDeleteRevisionReportsTheFailure checks that a failure tells which revision it was
// (pruning loops over several revisions, so without the name there is no telling where it
// stopped).
func TestClientDeleteRevisionReportsTheFailure(t *testing.T) {
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) {
		return http.StatusForbidden, googleAPIError(403, "denied")
	})

	err := c.DeleteRevision(context.Background(), "my-svc-00002-pqr")
	if err == nil || !strings.Contains(err.Error(), "my-svc-00002-pqr") {
		t.Errorf("DeleteRevision() error = %v, want it to name the revision", err)
	}
}

// TestSelectPrunableRevisionsKeepsPinnedRevisions checks that a revision named in spec.traffic is
// kept (even when status shows no share for it). During a rollout the shares on the status side
// may not appear, and looking only at those makes "every revision 0%".
func TestSelectPrunableRevisionsKeepsPinnedRevisions(t *testing.T) {
	all := Revisions{
		{Name: "my-svc-00004-abc"},
		{Name: "my-svc-00003-def", Pinned: true}, // named in spec.traffic, still 0% in status
		{Name: "my-svc-00002-ghi"},
	}
	var got []string
	for _, r := range SelectPrunableRevisions(all, 1) {
		got = append(got, r.Name)
	}
	if strings.Join(got, ",") != "my-svc-00002-ghi" {
		t.Errorf("SelectPrunableRevisions() = %v, want the pinned revision kept", got)
	}
}

// TestListRevisionsMarksPinnedRevisions checks that a revision named in spec.traffic is picked up
// as Pinned.
func TestListRevisionsMarksPinnedRevisions(t *testing.T) {
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if strings.HasSuffix(r.URL.Path, "/revisions") {
			return http.StatusOK, &run.ListRevisionsResponse{Items: []*run.Revision{
				revision("my-svc-00007-abc", "img", "2026-08-22T10:00:00Z", conditionTrue, ""),
				revision("my-svc-00006-def", "img", "2026-08-21T09:00:00Z", conditionTrue, ""),
			}}
		}
		svc := readyService()
		svc.Spec = &run.ServiceSpec{
			Traffic: []*run.TrafficTarget{{RevisionName: "my-svc-00006-def", Percent: 100}},
		}
		svc.Status = &run.ServiceStatus{} // the split is not reflected yet
		return http.StatusOK, svc
	})

	got, err := c.ListRevisions(context.Background(), "my-svc")
	if err != nil {
		t.Fatalf("ListRevisions() error = %v", err)
	}
	for _, r := range got {
		want := r.Name == "my-svc-00006-def"
		if r.Pinned != want {
			t.Errorf("%s: Pinned = %v, want %v", r.Name, r.Pinned, want)
		}
	}
}

// TestListRevisionsProtectsTheLatestWhileRollingOut checks that, when spec.traffic is
// latestRevision: true, a new revision whose share does not show in status yet is protected.
//
// latestRevision names no revision, so looking only at the names in spec puts "the revision about
// to serve" on the delete list with Percent 0 / Pinned false. This is the path by which running
// --prune --keep 0 during a rollout deletes it.
func TestListRevisionsProtectsTheLatestWhileRollingOut(t *testing.T) {
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if strings.HasSuffix(r.URL.Path, "/revisions") {
			return http.StatusOK, &run.ListRevisionsResponse{Items: []*run.Revision{
				revision("my-svc-00008-ghi", "img", "2026-08-23T11:00:00Z", "Unknown", "Deploying"),
				revision("my-svc-00007-abc", "img", "2026-08-22T10:00:00Z", conditionTrue, ""),
			}}
		}
		svc := readyService()
		svc.Spec = &run.ServiceSpec{Traffic: []*run.TrafficTarget{{LatestRevision: true, Percent: 100}}}
		// Mid-rollout: the new revision has been created, but the split is not reflected yet.
		svc.Status = &run.ServiceStatus{
			LatestCreatedRevisionName: "my-svc-00008-ghi",
			LatestReadyRevisionName:   "my-svc-00007-abc",
		}
		return http.StatusOK, svc
	})

	got, err := c.ListRevisions(context.Background(), "my-svc")
	if err != nil {
		t.Fatalf("ListRevisions() error = %v", err)
	}
	for _, r := range got {
		if !r.Pinned {
			t.Errorf("%s: Pinned = false, want the latest revisions protected while latestRevision is in spec", r.Name)
		}
	}
	if prunable := SelectPrunableRevisions(got, 0); len(prunable) != 0 {
		t.Errorf("SelectPrunableRevisions(keep=0) = %+v, want nothing prunable mid-rollout", prunable)
	}
}

// TestPinnedRevisionNamesIgnoresTheLatestWhenTrafficIsPinned checks that, while traffic is pinned
// by name (after a rollback), the latest revision is not additionally protected. Protecting that
// far means a revision left behind by a rollback can never be pruned.
func TestPinnedRevisionNamesIgnoresTheLatestWhenTrafficIsPinned(t *testing.T) {
	svc := &run.Service{
		Spec: &run.ServiceSpec{
			Traffic: []*run.TrafficTarget{{RevisionName: "my-svc-00006-def", Percent: 100}},
		},
		Status: &run.ServiceStatus{
			LatestCreatedRevisionName: "my-svc-00008-ghi",
			LatestReadyRevisionName:   "my-svc-00008-ghi",
		},
	}
	got := pinnedRevisionNames(svc)
	if !got["my-svc-00006-def"] {
		t.Error("the revision named in spec.traffic is not protected")
	}
	if got["my-svc-00008-ghi"] {
		t.Error("the latest revision is protected even though spec.traffic pins a name")
	}
}
