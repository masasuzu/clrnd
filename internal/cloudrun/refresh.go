package cloudrun

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	run "google.golang.org/api/run/v1"
)

// リビジョン名に対する Cloud Run の制約。実 API のエラーで確認したもの:
//   - "The revision name must be prefixed by the name of the enclosing Service with a trailing -"
//   - "only lowercase, digits, and hyphens; must begin with letter, and may not end with
//     hyphen; must be less than 64 characters."
const maxRevisionNameLen = 63

var revisionNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$`)

// RefreshSuffix は refresh が既定で使うリビジョン名のサフィックスを返す。
// 秒までの UTC タイムスタンプにするのは、人が見て「いつ流したか」が分かり、
// かつ連続実行でも衝突しないため (同名リビジョンは再作成できない)。
func RefreshSuffix(now time.Time) string {
	return "r" + now.UTC().Format("060102150405")
}

// RefreshTarget は live サービスに新しいリビジョン名を付けた定義を返す。
// 引数は書き換えず、変更が必要な Spec / Template / Metadata の経路だけを浅くコピーする。
//
// clrnd は原則としてリビジョン名を管理しない (init は落とし、diff は無視する) が、
// refresh だけは例外。Cloud Run は spec.template が変わらないと新しいリビジョンを
// 作らないため、「定義を変えずに流し直す」にはリビジョン名を明示するしかない。
// ここで付けた名前は次の deploy (リビジョン名を持たないマニフェスト) で消える。
func RefreshTarget(live *run.Service, service, suffix string) (*run.Service, error) {
	if live == nil || live.Spec == nil || live.Spec.Template == nil {
		return nil, errors.New("the live service has no spec.template to refresh")
	}
	if suffix == "" {
		return nil, errors.New("no revision suffix to apply")
	}

	name := service + "-" + suffix
	if err := validateRevisionName(name); err != nil {
		return nil, err
	}
	// 同じ名前では新しいリビジョンが作られない。差分がゼロになって
	// "No changes." で成功してしまい、流し直したつもりが何も起きない。
	if revisionName(live) == name {
		return nil, fmt.Errorf(
			"revision %q is already the current template revision, so refreshing would do nothing; "+
				"wait a second or pass a different --revision-suffix", name)
	}
	// トラフィックが特定のリビジョンへ固定されていると、新しいリビジョンは作られるが
	// 何も配信しない。rollback の直後がこの状態になる。refresh の目的を果たせないので断る。
	if !servesLatestRevision(live) {
		return nil, fmt.Errorf(
			"refresh would create a revision that receives no traffic: this service pins traffic to " +
				"specific revisions, so nothing follows the latest one; deploy the change you want, " +
				"or send traffic back to the latest revision first")
	}

	meta := run.ObjectMeta{}
	if live.Spec.Template.Metadata != nil {
		meta = *live.Spec.Template.Metadata
	}
	meta.Name = name

	template := *live.Spec.Template
	template.Metadata = &meta
	spec := *live.Spec
	spec.Template = &template
	out := *live
	out.Spec = &spec
	return &out, nil
}

// servesLatestRevision は「最新リビジョンへトラフィックが向くか」を返す。
// spec.traffic が未指定なら Cloud Run の既定 (latestRevision に 100%) と同じなので真。
// rollback はトラフィックを特定のリビジョンへ固定するため、その後は偽になる。
func servesLatestRevision(live *run.Service) bool {
	if len(live.Spec.Traffic) == 0 {
		return true
	}
	for _, t := range live.Spec.Traffic {
		// 割合 0 のタグ専用エントリは配信しないので数えない。
		if t != nil && t.LatestRevision && t.Percent > 0 {
			return true
		}
	}
	return false
}

// validateRevisionName は Cloud Run が拒否する名前を手元で弾く。サーバに投げても
// 同じ結果になるが、何が悪いのかを先に、分かる言葉で伝えるため。
func validateRevisionName(name string) error {
	if len(name) > maxRevisionNameLen {
		return fmt.Errorf(
			"revision name %q is %d characters; Cloud Run allows at most %d, so pass a shorter --revision-suffix",
			name, len(name), maxRevisionNameLen)
	}
	if !revisionNamePattern.MatchString(name) {
		return fmt.Errorf(
			"revision name %q is not valid; Cloud Run allows lowercase letters, digits and hyphens, "+
				"starting with a letter and not ending with a hyphen", name)
	}
	return nil
}
