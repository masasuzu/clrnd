// Package cloudrun は Cloud Run Admin API へのアクセスとマニフェストの整形を提供する。
package cloudrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pmezard/go-difflib/difflib"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	run "google.golang.org/api/run/v1"
	"sigs.k8s.io/yaml"
)

const (
	manifestAPIVersion = "serving.knative.dev/v1"
	manifestKind       = "Service"
)

// サーバ側が付与する read-only なアノテーション。デプロイ用マニフェストには不要。
var serverManagedAnnotations = []string{
	"run.googleapis.com/operation-id",
	"run.googleapis.com/ingress-status",
	"run.googleapis.com/urls",
	"serving.knative.dev/creator",
	"serving.knative.dev/lastModifier",
}

// サーバ側が付与する read-only なラベル。
var serverManagedLabels = []string{
	"client.knative.dev/nonce",
	"run.googleapis.com/startupProbeType",
}

// metadata 直下の read-only フィールド。
var serverManagedMetaFields = []string{
	"creationTimestamp",
	"generation",
	"resourceVersion",
	"selfLink",
	"uid",
	"namespace",
}

// newClient はローカルの Application Default Credentials を使う Cloud Run Admin API
// クライアントを生成する。ADC は run.NewService が自動的に検出する。
// v1 namespaces API はリージョナルエンドポイントを必要とする。
func newClient(ctx context.Context, region string) (*run.APIService, error) {
	endpoint := fmt.Sprintf("https://%s-run.googleapis.com", region)
	client, err := run.NewService(ctx, option.WithEndpoint(endpoint))
	if err != nil {
		return nil, fmt.Errorf("failed to initialize the Cloud Run client: %w", err)
	}
	return client, nil
}

// GetService は指定したサービスの定義を Cloud Run Admin API から取得する。
func GetService(ctx context.Context, project, region, service string) (*run.Service, error) {
	client, err := newClient(ctx, region)
	if err != nil {
		return nil, err
	}

	name := fmt.Sprintf("namespaces/%s/services/%s", project, service)
	obj, err := client.Namespaces.Services.Get(name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to get service %q: %w", service, err)
	}
	return obj, nil
}

// Deploy はマニフェストを Cloud Run に適用する。サービスが存在しなければ作成し、存在すれば
// 置換する。created は新規作成だったかを表す。dryRun が true の場合はサーバ側で検証のみ行い、
// 実際の変更は行わない。
func Deploy(ctx context.Context, project, region, service string, manifest []byte, dryRun bool) (created bool, err error) {
	svc, err := parseManifest(manifest)
	if err != nil {
		return false, err
	}
	if err := validate(svc, service); err != nil {
		return false, err
	}
	// 送信先プロジェクトと body の namespace を一致させる。
	if svc.Metadata != nil {
		svc.Metadata.Namespace = project
	}

	client, err := newClient(ctx, region)
	if err != nil {
		return false, err
	}

	var dryRunVal string
	if dryRun {
		dryRunVal = "all"
	}

	name := fmt.Sprintf("namespaces/%s/services/%s", project, service)
	_, getErr := client.Namespaces.Services.Get(name).Context(ctx).Do()
	if getErr != nil {
		if !isNotFound(getErr) {
			return false, fmt.Errorf("failed to check service %q: %w", service, getErr)
		}
		// 未存在なので新規作成する。
		parent := fmt.Sprintf("namespaces/%s", project)
		if _, err := client.Namespaces.Services.Create(parent, svc).DryRun(dryRunVal).Context(ctx).Do(); err != nil {
			return false, fmt.Errorf("failed to create service %q: %w", service, err)
		}
		return true, nil
	}

	// 既存なので置換する。
	if _, err := client.Namespaces.Services.ReplaceService(name, svc).DryRun(dryRunVal).Context(ctx).Do(); err != nil {
		return false, fmt.Errorf("failed to update service %q: %w", service, err)
	}
	return false, nil
}

// isNotFound は googleapi の 404 エラーかどうかを判定する。
func isNotFound(err error) bool {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return gerr.Code == 404
	}
	return false
}

// ToManifest はサーバ側が付与する read-only フィールドを取り除き、デプロイに使える
// Knative 形式の YAML マニフェストを返す。
func ToManifest(obj *run.Service) ([]byte, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to sanitize the manifest: %w", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("failed to sanitize the manifest: %w", err)
	}
	sanitizeMap(m)

	manifest, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("failed to convert the manifest to YAML: %w", err)
	}
	return manifest, nil
}

// Normalize はローカルのマニフェスト YAML を、リモート取得時 (ToManifest) と同じ正規化
// (read-only フィールド除去・キー整列) にそろえる。diff を公平に比較するために使う。
func Normalize(manifest []byte) ([]byte, error) {
	var m map[string]interface{}
	if err := yaml.Unmarshal(manifest, &m); err != nil {
		return nil, fmt.Errorf("failed to parse the manifest: %w", err)
	}
	sanitizeMap(m)

	out, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("failed to convert the manifest to YAML: %w", err)
	}
	return out, nil
}

// Validate はローカルのマニフェストが Cloud Run のサービス定義として妥当かを検証する。
// API へはアクセスせず、構造とデプロイに必須のフィールドだけを確認する。問題が無ければ
// nil を、複数の問題があればまとめたエラーを返す。
func Validate(manifest []byte, service string) error {
	svc, err := parseManifest(manifest)
	if err != nil {
		return err
	}
	return validate(svc, service)
}

// parseManifest はマニフェストを run.Service に厳密にパースする。UnmarshalStrict は
// 未知フィールド (フィールド名の打ち間違いなど) も検出する。
func parseManifest(manifest []byte) (*run.Service, error) {
	var svc run.Service
	if err := yaml.UnmarshalStrict(manifest, &svc); err != nil {
		return nil, fmt.Errorf("failed to parse the manifest: %w", err)
	}
	return &svc, nil
}

// validate はパース済みのサービス定義を検証する。
func validate(svc *run.Service, service string) error {
	var errs []error
	if svc.ApiVersion != manifestAPIVersion {
		errs = append(errs, fmt.Errorf("apiVersion must be %q, got %q", manifestAPIVersion, svc.ApiVersion))
	}
	if svc.Kind != manifestKind {
		errs = append(errs, fmt.Errorf("kind must be %q, got %q", manifestKind, svc.Kind))
	}

	switch {
	case svc.Metadata == nil || svc.Metadata.Name == "":
		errs = append(errs, errors.New("metadata.name is required"))
	case svc.Metadata.Name != service:
		errs = append(errs, fmt.Errorf("metadata.name %q does not match service argument %q", svc.Metadata.Name, service))
	}

	containers := serviceContainers(svc)
	if len(containers) == 0 {
		errs = append(errs, errors.New("spec.template.spec.containers must define at least one container"))
	}
	for i, c := range containers {
		switch {
		case c == nil:
			errs = append(errs, fmt.Errorf("spec.template.spec.containers[%d] must not be null", i))
		case c.Image == "":
			errs = append(errs, fmt.Errorf("spec.template.spec.containers[%d].image is required", i))
		}
	}

	return errors.Join(errs...)
}

// serviceContainers はサービス定義からコンテナ一覧を nil セーフに取り出す。
func serviceContainers(svc *run.Service) []*run.Container {
	if svc.Spec == nil || svc.Spec.Template == nil || svc.Spec.Template.Spec == nil {
		return nil
	}
	return svc.Spec.Template.Spec.Containers
}

// Diff は current と desired の統一 diff を返す。差分が無ければ空文字列を返す。
func Diff(current, desired []byte, currentName, desiredName string) (string, error) {
	d := difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(current)),
		B:        difflib.SplitLines(string(desired)),
		FromFile: currentName,
		ToFile:   desiredName,
		Context:  3,
	}
	out, err := difflib.GetUnifiedDiffString(d)
	if err != nil {
		return "", fmt.Errorf("failed to compute the diff: %w", err)
	}
	return out, nil
}

// sanitizeMap はサーバ側が付与する read-only なフィールドを map から取り除く。
func sanitizeMap(m map[string]interface{}) {
	// status はすべてサーバ側の状態情報なので丸ごと削除する。
	delete(m, "status")

	// metadata 直下の read-only フィールドとサーバ管理アノテーションを削除する。
	if meta, ok := m["metadata"].(map[string]interface{}); ok {
		for _, k := range serverManagedMetaFields {
			delete(meta, k)
		}
		deleteMapKeys(meta, "annotations", serverManagedAnnotations)
	}

	// spec.template.metadata のサーバ管理ラベル/アノテーションを削除する。
	if spec, ok := m["spec"].(map[string]interface{}); ok {
		if tmpl, ok := spec["template"].(map[string]interface{}); ok {
			if tmeta, ok := tmpl["metadata"].(map[string]interface{}); ok {
				deleteMapKeys(tmeta, "annotations", serverManagedAnnotations)
				deleteMapKeys(tmeta, "labels", serverManagedLabels)
			}
		}
	}
}

// deleteMapKeys は parent[field] (map) から指定キーを削除し、空になったら field 自体も削除する。
func deleteMapKeys(parent map[string]interface{}, field string, keys []string) {
	child, ok := parent[field].(map[string]interface{})
	if !ok {
		return
	}
	for _, k := range keys {
		delete(child, k)
	}
	if len(child) == 0 {
		delete(parent, field)
	}
}
