package cloudrun

import (
	"errors"
	"fmt"

	run "google.golang.org/api/run/v1"
)

// TrafficRequest は traffic コマンドの指定。Revision と Latest はどちらか一方だけ。
type TrafficRequest struct {
	// Revision は送り先のリビジョン名。
	Revision string
	// Latest は「最新のリビジョン」を送り先にする指定 (latestRevision: true)。
	// リビジョン名を固定しないので、以降の deploy が作る版へ自動的に追従する。
	Latest bool
	// Percent は送り先に向ける割合 (1..100)。100 未満なら残りは現在いちばん多く
	// 受けているリビジョンに残す。
	Percent int64
}

// ValidateTrafficRequest は指定の組み合わせを検証する。API アクセスを伴わないので、
// クライアントを作る前 (= 認証情報を探す前) に呼べる。フラグの間違いが認証エラーの
// 後ろに隠れないようにするため、cmd 側はこれを先に通す。
func ValidateTrafficRequest(req TrafficRequest) error {
	switch {
	case req.Latest && req.Revision != "":
		return errors.New("--to and --to-latest cannot be combined")
	case !req.Latest && req.Revision == "":
		return errors.New("no revision to send traffic to: pass --to or --to-latest")
	case req.Percent <= 0 || req.Percent > 100:
		return fmt.Errorf("--percent must be between 1 and 100, got %d", req.Percent)
	}
	return nil
}

// ShiftTrafficTarget は live サービスのトラフィック配分だけを書き換えた定義を返す
// (名前が Target で終わるのは RollbackTarget / RefreshTarget と同じく「適用する
// desired を組み立てる純粋な関数」であることを示すため)。
// 引数は書き換えず、変更が必要な Spec だけを浅くコピーする。
//
// spec.template には触らないので新しいリビジョンは作られない。カナリア (--percent 10)
// も、rollback 後に最新へ戻す操作 (--to-latest) も、この 1 つの経路で表現できる。
//
// 残りの割合は「いま最も多く受けているリビジョン」に寄せる。カナリアの実際の使い方
// (安定版 90% / 新版 10%) がその形であり、結果が常に 2 つのリビジョンの分割になるので
// 実行前に何が起きるか読める。既存の配分を比例で保つ方式は、丸め誤差の扱いが要るうえ
// 何が残るのか予想しにくい。
func ShiftTrafficTarget(live *run.Service, req TrafficRequest) (*run.Service, error) {
	if live == nil || live.Spec == nil {
		return nil, errors.New("the live service has no spec to update")
	}
	if err := ValidateTrafficRequest(req); err != nil {
		return nil, err
	}

	status := newStatus(live)

	target := &run.TrafficTarget{Percent: req.Percent}
	// 残りの受け皿を選ぶときに、送り先そのものを候補から外すための名前。
	// --to-latest では「いま最新の Ready なリビジョン」がそれにあたる。
	self := req.Revision
	if req.Latest {
		target.LatestRevision = true
		self = status.LatestReadyRevision
	} else {
		target.RevisionName = req.Revision
	}

	traffic := []*run.TrafficTarget{target}
	if req.Percent < 100 {
		rest := largestShareExcept(status, self)
		if rest == "" {
			return nil, fmt.Errorf(
				"no other revision is serving traffic to keep the remaining %d%%; pass --percent 100",
				100-req.Percent)
		}
		traffic = append(traffic, &run.TrafficTarget{RevisionName: rest, Percent: 100 - req.Percent})
	}

	// タグ付きの経路は割合 0 で残す。タグ URL でのアクセス手段を、割合の変更で
	// 失わせない (rollback と同じ扱い)。
	for _, t := range live.Spec.Traffic {
		if t == nil || t.Tag == "" {
			continue
		}
		kept := *t
		kept.Percent = 0
		traffic = append(traffic, &kept)
	}

	spec := *live.Spec
	spec.Traffic = traffic
	out := *live
	out.Spec = &spec
	return &out, nil
}

// largestShareExcept は except 以外でいま最も多くトラフィックを受けているリビジョンを返す。
// 同率のときは名前の昇順で決める (実行のたびに結果が変わらないようにするため)。
// 該当が無ければ空文字列。
func largestShareExcept(s *Status, except string) string {
	shares := make(map[string]int64)
	if s != nil {
		for _, t := range s.Traffic {
			if t.RevisionName == "" || t.RevisionName == except {
				continue
			}
			shares[t.RevisionName] += t.Percent
		}
	}

	best, bestShare := "", int64(0)
	for name, share := range shares {
		if share <= 0 {
			continue
		}
		if share > bestShare || (share == bestShare && name < best) {
			best, bestShare = name, share
		}
	}
	return best
}

// pinnedTraffic は live サービスの *いまの* 配分を、リビジョン名で固定した spec.traffic
// として返す。deploy --no-traffic のためのもので、latestRevision: true のままだと
// これから作るリビジョンへ全量が移ってしまうため、具体的な名前に置き換える。
func pinnedTraffic(live *run.Service) []*run.TrafficTarget {
	status := newStatus(live)
	var out []*run.TrafficTarget
	for _, t := range status.Traffic {
		if t.RevisionName == "" {
			continue
		}
		out = append(out, &run.TrafficTarget{
			RevisionName: t.RevisionName,
			Tag:          t.Tag,
			Percent:      t.Percent,
		})
	}
	return out
}

// HasTraffic はマニフェストが spec.traffic を明示しているかを返す。--no-traffic が
// それを置き換えることを警告するために使う (パースだけで API は使わない)。
func HasTraffic(manifest []byte) (bool, error) {
	svc, err := parseManifest(manifest)
	if err != nil {
		return false, err
	}
	return svc.Spec != nil && len(svc.Spec.Traffic) > 0, nil
}
