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

// revision はテスト用のリビジョンを組み立てる。ready が空なら Ready 条件を持たない。
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

// statusWithTraffic は status.traffic だけを持つ Status を組み立てる。
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
	// 新しい順。
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
	// 同じリビジョンが「割合」と「タグ」で複数エントリに現れることがある。
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

// TestNewRevisionsKeepsEveryContainerImage は、サイドカーを持つリビジョンの
// イメージが全部出ることを確認する。最初の 1 つだけを返していたころは、
// 表示されていないイメージが動いている状態になっていた。
func TestNewRevisionsKeepsEveryContainerImage(t *testing.T) {
	item := revision("my-svc-00007-abc", "gcr.io/p/app:v2", "2026-08-22T10:00:00Z", conditionTrue, "")
	item.Spec.Containers = append(item.Spec.Containers,
		&run.Container{Image: ""}, // イメージの無いコンテナは飛ばす
		&run.Container{Image: "gcr.io/p/proxy:v1"})

	got := newRevisions([]*run.Revision{item}, nil, nil)
	want := []string{"gcr.io/p/app:v2", "gcr.io/p/proxy:v1"}
	if strings.Join(got[0].Images, ",") != strings.Join(want, ",") {
		t.Errorf("Images = %v, want %v in spec order", got[0].Images, want)
	}
	// 既存の JSON 利用 (jq '.[].image') を壊さないため、image は残して先頭を指す。
	if got[0].Image != want[0] {
		t.Errorf("Image = %q, want the first container %q", got[0].Image, want[0])
	}
	// 表の IMAGE 列にも両方出る。
	text := Revisions{got[0]}.Text()
	if !strings.Contains(text, "gcr.io/p/app:v2,gcr.io/p/proxy:v1") {
		t.Errorf("Text() = %q, want both images in the IMAGE column", text)
	}
}

func TestNewRevisionsIsNilSafe(t *testing.T) {
	items := []*run.Revision{
		nil,
		{},                                     // metadata も spec も無い
		{Metadata: &run.ObjectMeta{Name: "x"}}, // spec が無い
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
	// 作成時刻が読めない場合は名前の降順 (Cloud Run の採番は連番)。
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

// TestSortRevisionsHandlesFractionalSeconds は実 API が返す形式で並べられることを
// 確認する。Cloud Run の creationTimestamp は "2026-08-22T17:51:38.143198Z" のように
// 小数秒を含む。
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
	// live のトラフィックが突き合わされている。
	if got[0].Percent != 100 {
		t.Errorf("Percent = %d, want 100 from the service traffic", got[0].Percent)
	}
	// サービスとリビジョンの両方を引いている。
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

// TestClientListRevisionsFailsOnARepeatedToken は、サーバが同じ Continue トークンを
// 返し続けたときに、そこまでの一覧を成功として返さずエラーにすることを確認する。
// 進んでいない応答を追い続けると items が際限なく伸びるので打ち切りは必要だが、
// 打ち切った一覧は重複も欠落もありうる。rollback がそれを完全な一覧として扱うと
// 誤った版へ戻しうるので、黙って切り詰めない。
func TestClientListRevisionsFailsOnARepeatedToken(t *testing.T) {
	pages := 0
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if !strings.HasSuffix(r.URL.Path, "/revisions") {
			return http.StatusOK, readyService()
		}
		pages++
		// 常に同じトークンを返す (進まないページング)。
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
	// 1 ページ目と、同じトークンを持ってきた 2 ページ目で打ち切る。
	if pages != 2 {
		t.Errorf("requested %d pages, want it to stop at 2", pages)
	}
}

// TestClientListRevisionsFailsAtThePageLimit は、上限ページ数に達してもトークンが
// 残っている場合に、読めたところまでを成功として返さないことを確認する。
func TestClientListRevisionsFailsAtThePageLimit(t *testing.T) {
	pages := 0
	c, _ := newTestClient(t, func(r *http.Request) (int, interface{}) {
		if !strings.HasSuffix(r.URL.Path, "/revisions") {
			return http.StatusOK, readyService()
		}
		pages++
		// 毎回違うトークンを返す (進んではいるが終わらないページング)。
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

// TestSelectPrunableRevisions は掃除の対象選びを確認する。消してはいけないものを
// 消さないことがこの関数の要件。
func TestSelectPrunableRevisions(t *testing.T) {
	// 新しい順。00005 が配信中、00003 にタグが付いている。
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

// TestSelectPrunableRevisionsCountsProtectedTowardKeep は、保護されたリビジョンも
// 保持数に数えることを確認する。数え方を変えると、--keep 3 と指定したのに残る数が
// 増減して、指定と結果が一致しなくなる。
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

// TestClientDeleteRevisionUsesTheNamespacedName は、削除がリビジョンのリソース名を
// 組み立てて呼ばれることを確認する。
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

// TestClientDeleteRevisionReportsTheFailure は、失敗時にどのリビジョンかが分かる
// ことを確認する (掃除は複数件を回すので、名前が出ないとどこで止まったか分からない)。
func TestClientDeleteRevisionReportsTheFailure(t *testing.T) {
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) {
		return http.StatusForbidden, googleAPIError(403, "denied")
	})

	err := c.DeleteRevision(context.Background(), "my-svc-00002-pqr")
	if err == nil || !strings.Contains(err.Error(), "my-svc-00002-pqr") {
		t.Errorf("DeleteRevision() error = %v, want it to name the revision", err)
	}
}

// TestSelectPrunableRevisionsKeepsPinnedRevisions は、spec.traffic が名指ししている
// リビジョンを (status に割合が出ていなくても) 残すことを確認する。ロールアウト中は
// status 側の割合が現れないことがあり、そこだけを見ると「どれも 0%」に見えてしまう。
func TestSelectPrunableRevisionsKeepsPinnedRevisions(t *testing.T) {
	all := Revisions{
		{Name: "my-svc-00004-abc"},
		{Name: "my-svc-00003-def", Pinned: true}, // spec.traffic が名指し、status はまだ 0%
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

// TestListRevisionsMarksPinnedRevisions は、spec.traffic の名指しが Pinned として
// 拾われることを確認する。
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
		svc.Status = &run.ServiceStatus{} // まだ配分が反映されていない状態
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
