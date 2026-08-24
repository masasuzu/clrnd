package cloudrun

import (
	"strings"
	"testing"

	run "google.golang.org/api/run/v1"
)

// serviceWithTraffic は spec.traffic と status.traffic を持つ live サービスを組み立てる。
// spec 側は「いま宣言されている配分」、status 側は「実際に配られている配分」で、
// 残りの受け皿を選ぶのは後者を見る。
func serviceWithTraffic(spec []*run.TrafficTarget, status []*run.TrafficTarget, latestReady string) *run.Service {
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
			LatestReadyRevisionName: latestReady,
			Traffic:                 status,
		},
	}
}

// TestShiftTrafficTargetSplitsAgainstTheLargestShare は、割合を 100 未満にしたときに
// 残りが「いちばん多く受けているリビジョン」に寄ることを確認する。カナリアの形。
func TestShiftTrafficTargetSplitsAgainstTheLargestShare(t *testing.T) {
	live := serviceWithTraffic(
		[]*run.TrafficTarget{{RevisionName: "my-svc-00007-abc", Percent: 100}},
		[]*run.TrafficTarget{
			{RevisionName: "my-svc-00007-abc", Percent: 90},
			{RevisionName: "my-svc-00006-def", Percent: 10},
		},
		"my-svc-00008-ghi")

	got, err := ShiftTrafficTarget(live, TrafficRequest{Revision: "my-svc-00008-ghi", Percent: 20})
	if err != nil {
		t.Fatalf("ShiftTrafficTarget() error = %v", err)
	}
	want := []*run.TrafficTarget{
		{RevisionName: "my-svc-00008-ghi", Percent: 20},
		{RevisionName: "my-svc-00007-abc", Percent: 80},
	}
	assertTraffic(t, got.Spec.Traffic, want)
}

// TestShiftTrafficTargetToLatestFollowsTheNewest は、--to-latest がリビジョン名を
// 固定せず latestRevision を立てることを確認する。rollback が固定したトラフィックを
// 最新へ戻す (行き止まりから抜ける) のがこの経路。
func TestShiftTrafficTargetToLatestFollowsTheNewest(t *testing.T) {
	live := serviceWithTraffic(
		[]*run.TrafficTarget{{RevisionName: "my-svc-00006-def", Percent: 100}},
		[]*run.TrafficTarget{{RevisionName: "my-svc-00006-def", Percent: 100}},
		"my-svc-00008-ghi")

	got, err := ShiftTrafficTarget(live, TrafficRequest{Latest: true, Percent: 100})
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

// TestShiftTrafficTargetToLatestSplitsAgainstTheStableRevision は、--to-latest でも
// 残りが最新以外のリビジョンに寄ることを確認する (最新自身を受け皿にすると、同じ版に
// 2 つのエントリが向くだけで分割にならない)。
func TestShiftTrafficTargetToLatestSplitsAgainstTheStableRevision(t *testing.T) {
	live := serviceWithTraffic(
		nil,
		[]*run.TrafficTarget{
			{RevisionName: "my-svc-00008-ghi", Percent: 100, LatestRevision: true},
			{RevisionName: "my-svc-00007-abc", Percent: 0},
		},
		"my-svc-00008-ghi")

	_, err := ShiftTrafficTarget(live, TrafficRequest{Latest: true, Percent: 10})
	if err == nil {
		t.Fatal("ShiftTrafficTarget() error = nil, want it to refuse: nothing else is serving")
	}
	if !strings.Contains(err.Error(), "--percent 100") {
		t.Errorf("error = %v, want it to point at --percent 100", err)
	}
}

// TestShiftTrafficTargetKeepsTags は、タグ付きの経路が 0% で残ることを確認する。
// 割合を動かしただけでタグ URL が消えると、そこを指していた確認手段が失われる。
func TestShiftTrafficTargetKeepsTags(t *testing.T) {
	live := serviceWithTraffic(
		[]*run.TrafficTarget{
			{RevisionName: "my-svc-00007-abc", Percent: 100},
			{RevisionName: "my-svc-00006-def", Percent: 0, Tag: "previous"},
		},
		[]*run.TrafficTarget{{RevisionName: "my-svc-00007-abc", Percent: 100}},
		"my-svc-00007-abc")

	got, err := ShiftTrafficTarget(live, TrafficRequest{Revision: "my-svc-00006-def", Percent: 100})
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

// TestShiftTrafficTargetLeavesTheTemplateAlone は、テンプレートに触らないこと
// (= 新しいリビジョンを作らないこと) と、引数を書き換えないことを確認する。
func TestShiftTrafficTargetLeavesTheTemplateAlone(t *testing.T) {
	live := serviceWithTraffic(
		[]*run.TrafficTarget{{RevisionName: "my-svc-00007-abc", Percent: 100}},
		[]*run.TrafficTarget{{RevisionName: "my-svc-00007-abc", Percent: 100}},
		"my-svc-00007-abc")

	got, err := ShiftTrafficTarget(live, TrafficRequest{Revision: "my-svc-00006-def", Percent: 100})
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

func TestValidateTrafficRequest(t *testing.T) {
	tests := []struct {
		name string
		req  TrafficRequest
		want string // エラーに含まれてほしい文字列。空なら成功。
	}{
		{"revision", TrafficRequest{Revision: "my-svc-00007-abc", Percent: 100}, ""},
		{"latest", TrafficRequest{Latest: true, Percent: 50}, ""},
		{"both", TrafficRequest{Revision: "r", Latest: true, Percent: 100}, "cannot be combined"},
		{"neither", TrafficRequest{Percent: 100}, "--to or --to-latest"},
		{"zero percent", TrafficRequest{Latest: true}, "between 1 and 100"},
		{"over 100", TrafficRequest{Latest: true, Percent: 101}, "between 1 and 100"},
		{"negative", TrafficRequest{Latest: true, Percent: -1}, "between 1 and 100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTrafficRequest(tt.req)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("ValidateTrafficRequest() error = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("ValidateTrafficRequest() error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestLargestShareExceptIsDeterministic は、同率のときに結果が実行のたびに変わらない
// ことを確認する (map の反復順に引きずられると、同じ入力で別のリビジョンが選ばれる)。
func TestLargestShareExceptIsDeterministic(t *testing.T) {
	status := statusWithTraffic(
		TrafficTarget{RevisionName: "my-svc-00007-abc", Percent: 50},
		TrafficTarget{RevisionName: "my-svc-00006-def", Percent: 50},
	)
	for i := 0; i < 20; i++ {
		if got := largestShareExcept(status, "my-svc-00008-ghi"); got != "my-svc-00006-def" {
			t.Fatalf("largestShareExcept() = %q, want the name-ordered winner on every run", got)
		}
	}
}

// TestPinnedTrafficFixesTheLatestPointer は、deploy --no-traffic が使う固定処理が
// latestRevision を具体的なリビジョン名に置き換えることを確認する。ここが名前に
// ならないと、これから作るリビジョンが全量を受け取ってしまう。
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

func TestHasTraffic(t *testing.T) {
	with := []byte(validManifest + "  traffic:\n  - revisionName: my-svc-00007-abc\n    percent: 100\n")
	if ok, err := HasTraffic(with); err != nil || !ok {
		t.Errorf("HasTraffic(with traffic) = %v, %v; want true, nil", ok, err)
	}
	if ok, err := HasTraffic([]byte(validManifest)); err != nil || ok {
		t.Errorf("HasTraffic(without traffic) = %v, %v; want false, nil", ok, err)
	}
}

// assertTraffic は revisionName / tag / percent の並びが一致することを確かめる。
func assertTraffic(t *testing.T, got, want []*run.TrafficTarget) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("traffic = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].RevisionName != want[i].RevisionName ||
			got[i].Tag != want[i].Tag ||
			got[i].Percent != want[i].Percent {
			t.Errorf("traffic[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
