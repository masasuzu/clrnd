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
