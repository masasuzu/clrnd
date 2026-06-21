package cloudrun

import (
	"context"
	"fmt"
	"strings"

	iam "google.golang.org/api/iam/v1"
	run "google.golang.org/api/run/v1"
	secretmanager "google.golang.org/api/secretmanager/v1"
)

// RemoteCheck はリモート実在チェックの結果。Missing は実在しないと確定したリソースの説明
// (verify を失敗させる)。Unchecked は権限不足・API 未到達・認証なしなどで確認できなかった
// ものの説明 (verify を失敗させず、警告として扱う)。後者を失敗にすると、ambient な
// project/region を持つだけの CI のオフライン lint を壊してしまうため区別する。
type RemoteCheck struct {
	Missing   []string
	Unchecked []string
}

// VerifyRemote はマニフェストが参照するリソースが実在するかを API で確認する。Validate
// (ローカルなスキーマ検証) を補完するもので、サービスアカウントと Secret Manager の
// シークレットの実在を ADC で確認する。404 (実在しない) のみを Missing として返し、それ
// 以外のエラー (クライアント初期化失敗・権限不足・API 無効など) は Unchecked に振り分ける。
// region は将来のイメージ (Artifact Registry) チェック用に受け取るが、現状は未使用。
func VerifyRemote(ctx context.Context, project, region string, manifest []byte) (*RemoteCheck, error) {
	svc, err := parseManifest(manifest)
	if err != nil {
		return nil, err
	}

	sa := serviceAccountName(svc)
	secrets := secretNames(svc)
	res := &RemoteCheck{}

	if sa != "" {
		iamSvc, err := iam.NewService(ctx)
		if err != nil {
			res.Unchecked = append(res.Unchecked, fmt.Sprintf("service account %q: %v", sa, err))
		} else {
			name := fmt.Sprintf("projects/%s/serviceAccounts/%s", project, sa)
			if _, err := iamSvc.Projects.ServiceAccounts.Get(name).Context(ctx).Do(); err != nil {
				if isNotFound(err) {
					res.Missing = append(res.Missing, fmt.Sprintf("service account %q does not exist", sa))
				} else {
					res.Unchecked = append(res.Unchecked, fmt.Sprintf("service account %q: %v", sa, err))
				}
			}
		}
	}

	if len(secrets) > 0 {
		smSvc, err := secretmanager.NewService(ctx)
		if err != nil {
			for _, s := range secrets {
				res.Unchecked = append(res.Unchecked, fmt.Sprintf("secret %q: %v", s, err))
			}
		} else {
			for _, s := range secrets {
				name := secretResourceName(project, s)
				if _, err := smSvc.Projects.Secrets.Get(name).Context(ctx).Do(); err != nil {
					if isNotFound(err) {
						res.Missing = append(res.Missing, fmt.Sprintf("secret %q does not exist", s))
					} else {
						res.Unchecked = append(res.Unchecked, fmt.Sprintf("secret %q: %v", s, err))
					}
				}
			}
		}
	}

	// TODO: containers[].image の Artifact Registry / GCR 到達性チェック (第二段)。

	return res, nil
}

// serviceAccountName はマニフェストの実行サービスアカウントを nil セーフに取り出す。
func serviceAccountName(svc *run.Service) string {
	if svc.Spec == nil || svc.Spec.Template == nil || svc.Spec.Template.Spec == nil {
		return ""
	}
	return svc.Spec.Template.Spec.ServiceAccountName
}

// secretNames はマニフェストが参照する Secret Manager シークレット名を重複なく集める。
// env の secretKeyRef と secret ボリュームの両方を見る。
func secretNames(svc *run.Service) []string {
	if svc.Spec == nil || svc.Spec.Template == nil || svc.Spec.Template.Spec == nil {
		return nil
	}
	spec := svc.Spec.Template.Spec

	seen := make(map[string]bool)
	var out []string
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}

	for _, c := range spec.Containers {
		if c == nil {
			continue
		}
		for _, e := range c.Env {
			if e != nil && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				add(e.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
	for _, v := range spec.Volumes {
		if v != nil && v.Secret != nil {
			add(v.Secret.SecretName)
		}
	}
	return out
}

// secretResourceName はシークレット名を Secret Manager の resource 名に整える。
// 既に projects/.../secrets/... 形式ならそのまま (末尾の /versions/... は落とす)。
func secretResourceName(project, name string) string {
	if strings.HasPrefix(name, "projects/") {
		if i := strings.Index(name, "/versions/"); i >= 0 {
			return name[:i]
		}
		return name
	}
	return fmt.Sprintf("projects/%s/secrets/%s", project, name)
}
