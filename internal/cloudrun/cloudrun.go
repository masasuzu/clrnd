// Package cloudrun は Cloud Run Admin API へのアクセスとマニフェストの整形を提供する。
package cloudrun

import (
	"context"
	"encoding/json"
	"fmt"

	"google.golang.org/api/option"
	run "google.golang.org/api/run/v1"
	"sigs.k8s.io/yaml"
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

// GetService はローカルの Application Default Credentials を使い、指定したサービスの定義を
// Cloud Run Admin API から取得する。ADC は run.NewService が自動的に検出する。
func GetService(ctx context.Context, project, region, service string) (*run.Service, error) {
	// v1 namespaces API はリージョナルエンドポイントを必要とする。
	endpoint := fmt.Sprintf("https://%s-run.googleapis.com", region)
	client, err := run.NewService(ctx, option.WithEndpoint(endpoint))
	if err != nil {
		return nil, fmt.Errorf("failed to initialize the Cloud Run client: %w", err)
	}

	name := fmt.Sprintf("namespaces/%s/services/%s", project, service)
	obj, err := client.Namespaces.Services.Get(name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to get service %q: %w", service, err)
	}
	return obj, nil
}

// ToManifest はサーバ側が付与する read-only フィールドを取り除き、デプロイに使える
// Knative 形式の YAML マニフェストを返す。
func ToManifest(obj *run.Service) ([]byte, error) {
	cleaned, err := sanitize(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to sanitize the manifest: %w", err)
	}

	manifest, err := yaml.Marshal(cleaned)
	if err != nil {
		return nil, fmt.Errorf("failed to convert the manifest to YAML: %w", err)
	}
	return manifest, nil
}

// sanitize はサーバ側が付与する read-only なフィールドを取り除いた map を返す。
func sanitize(obj *run.Service) (map[string]interface{}, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}

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

	return m, nil
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
