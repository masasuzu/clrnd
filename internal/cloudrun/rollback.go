package cloudrun

import (
	"errors"
	"fmt"

	run "google.golang.org/api/run/v1"
)

// SelectRollbackRevision は戻し先のリビジョンを決める。revisions は ListRevisions が
// 返す「新しい順」であることを前提にする。
//
// requested が指定されていればそれを (このサービスのものか確認したうえで) 返す。
// 省略時は、いまトラフィックを受けているリビジョンより 1 つ古い Ready なリビジョンを選ぶ。
// トラフィックを失った古いリビジョンも Ready=True (Reason=Retired) のままなので、
// これで「直前に動いていた版」が選べる。
func SelectRollbackRevision(revisions Revisions, requested string) (*Revision, error) {
	if requested != "" {
		for i := range revisions {
			if revisions[i].Name == requested {
				return &revisions[i], nil
			}
		}
		return nil, fmt.Errorf("revision %q does not belong to this service", requested)
	}

	current := -1
	var highest int64
	for i, r := range revisions {
		if r.Percent > highest {
			highest, current = r.Percent, i
		}
	}
	if current < 0 {
		return nil, errors.New("no revision is currently receiving traffic; pass --revision to choose one")
	}

	for i := current + 1; i < len(revisions); i++ {
		if revisions[i].Ready == conditionTrue {
			return &revisions[i], nil
		}
	}
	return nil, fmt.Errorf("no ready revision older than %q to roll back to; pass --revision to choose one",
		revisions[current].Name)
}

// RollbackTarget は live サービスのトラフィックを revision へ 100% 振り向けた新しい
// サービス定義を返す。引数は書き換えず、変更が必要な Spec だけを浅くコピーする。
//
// spec.template には触らないので新しいリビジョンは作られない。タグ付きの経路は
// 割合 0 で残す (タグ URL でのアクセス手段を rollback で失わせない)。タグの無い
// 既存の配分 (latestRevision を含む) は戻し先に集約されるため落とす。
func RollbackTarget(live *run.Service, revision string) (*run.Service, error) {
	if live == nil || live.Spec == nil {
		return nil, errors.New("the live service has no spec to roll back")
	}
	if revision == "" {
		return nil, errors.New("no revision to roll back to")
	}

	traffic := []*run.TrafficTarget{{RevisionName: revision, Percent: 100}}
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
