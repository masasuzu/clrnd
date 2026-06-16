package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
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

var (
	loadProject string
	loadRegion  string
	loadOutput  string
)

var loadCmd = &cobra.Command{
	Use:   "load <service>",
	Short: "Load the manifest of an existing service",
	Long: "Access the Cloud Run Admin API using the local Application Default\n" +
		"Credentials and fetch the manifest (Knative-style YAML) of the given service.",
	Args: cobra.ExactArgs(1),
	RunE: runLoad,
}

func init() {
	loadCmd.Flags().StringVar(&loadProject, "project", "", "GCP project ID")
	loadCmd.Flags().StringVar(&loadRegion, "region", "", "Cloud Run region (e.g. asia-northeast1)")
	loadCmd.Flags().StringVarP(&loadOutput, "output", "o", "", "output file (stdout if not set)")
	_ = loadCmd.MarkFlagRequired("project")
	_ = loadCmd.MarkFlagRequired("region")
}

func runLoad(cmd *cobra.Command, args []string) error {
	serviceName := args[0]
	ctx := context.Background()

	// 区切りのリージョナルエンドポイントを使う。ADC は自動的に検出される。
	endpoint := fmt.Sprintf("https://%s-run.googleapis.com", loadRegion)
	client, err := run.NewService(ctx, option.WithEndpoint(endpoint))
	if err != nil {
		return fmt.Errorf("failed to initialize the Cloud Run client: %w", err)
	}

	name := fmt.Sprintf("namespaces/%s/services/%s", loadProject, serviceName)
	obj, err := client.Namespaces.Services.Get(name).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed to get service %q: %w", serviceName, err)
	}

	cleaned, err := sanitizeManifest(obj)
	if err != nil {
		return fmt.Errorf("failed to sanitize the manifest: %w", err)
	}

	manifest, err := yaml.Marshal(cleaned)
	if err != nil {
		return fmt.Errorf("failed to convert the manifest to YAML: %w", err)
	}

	if loadOutput != "" {
		if err := os.WriteFile(loadOutput, manifest, 0o644); err != nil {
			return fmt.Errorf("failed to write to %s: %w", loadOutput, err)
		}
		return nil
	}

	fmt.Fprint(cmd.OutOrStdout(), string(manifest))
	return nil
}

// sanitizeManifest はサーバ側が付与する read-only なフィールドを取り除き、
// デプロイに使える形のマニフェスト (map) を返す。
func sanitizeManifest(obj *run.Service) (map[string]interface{}, error) {
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
