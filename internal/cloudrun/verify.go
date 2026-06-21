package cloudrun

import (
	"context"
	"errors"
	"fmt"
	"strings"

	iam "google.golang.org/api/iam/v1"
	run "google.golang.org/api/run/v1"
	secretmanager "google.golang.org/api/secretmanager/v1"
)

// VerifyRemote はマニフェストが参照するリソースが実在するかを API で確認する。Validate
// (ローカルなスキーマ検証) を補完するもので、サービスアカウントと Secret Manager の
// シークレットの実在を ADC で確認する。問題は errors.Join でまとめて返す。
// region は将来のイメージ (Artifact Registry) チェック用に受け取るが、現状は未使用。
func VerifyRemote(ctx context.Context, project, region string, manifest []byte) error {
	svc, err := parseManifest(manifest)
	if err != nil {
		return err
	}

	sa := serviceAccountName(svc)
	secrets := secretNames(svc)

	var errs []error

	if sa != "" {
		iamSvc, err := iam.NewService(ctx)
		if err != nil {
			return fmt.Errorf("failed to initialize the IAM client: %w", err)
		}
		name := fmt.Sprintf("projects/%s/serviceAccounts/%s", project, sa)
		if _, err := iamSvc.Projects.ServiceAccounts.Get(name).Context(ctx).Do(); err != nil {
			if isNotFound(err) {
				errs = append(errs, fmt.Errorf("service account %q does not exist", sa))
			} else {
				errs = append(errs, fmt.Errorf("failed to check service account %q: %w", sa, err))
			}
		}
	}

	if len(secrets) > 0 {
		smSvc, err := secretmanager.NewService(ctx)
		if err != nil {
			return fmt.Errorf("failed to initialize the Secret Manager client: %w", err)
		}
		for _, s := range secrets {
			name := secretResourceName(project, s)
			if _, err := smSvc.Projects.Secrets.Get(name).Context(ctx).Do(); err != nil {
				if isNotFound(err) {
					errs = append(errs, fmt.Errorf("secret %q does not exist", s))
				} else {
					errs = append(errs, fmt.Errorf("failed to check secret %q: %w", s, err))
				}
			}
		}
	}

	// TODO: containers[].image の Artifact Registry / GCR 到達性チェック (第二段)。

	return errors.Join(errs...)
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
