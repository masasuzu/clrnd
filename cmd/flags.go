package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// プロジェクト/リージョンのフラグが未指定のときに参照する環境変数 (gcloud 互換)。
// 先のものを優先する。
const (
	envProjectPrimary   = "CLOUDSDK_CORE_PROJECT" // gcloud config core/project
	envProjectSecondary = "GOOGLE_CLOUD_PROJECT"  // Google クライアントライブラリ標準
	envRegionPrimary    = "CLOUDSDK_RUN_REGION"   // gcloud config run/region
	envRegionSecondary  = "GOOGLE_CLOUD_REGION"
)

// addTargetFlags は --project / --region フラグを登録する。これらは必須だが、未指定の
// 場合は環境変数にフォールバックするため MarkFlagRequired は使わず resolve* で検証する。
func addTargetFlags(cmd *cobra.Command, project, region *string) {
	cmd.Flags().StringVar(project, "project", "",
		fmt.Sprintf("GCP project ID (env: %s, %s)", envProjectPrimary, envProjectSecondary))
	cmd.Flags().StringVar(region, "region", "",
		fmt.Sprintf("Cloud Run region, e.g. asia-northeast1 (env: %s, %s)", envRegionPrimary, envRegionSecondary))
}

// resolveProject はフラグ値が空なら環境変数にフォールバックする。どちらも無ければエラー。
func resolveProject(flag string) (string, error) {
	if v := firstNonEmpty(flag, os.Getenv(envProjectPrimary), os.Getenv(envProjectSecondary)); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("project is required: set --project or $%s / $%s", envProjectPrimary, envProjectSecondary)
}

// resolveRegion はフラグ値が空なら環境変数にフォールバックする。どちらも無ければエラー。
func resolveRegion(flag string) (string, error) {
	if v := firstNonEmpty(flag, os.Getenv(envRegionPrimary), os.Getenv(envRegionSecondary)); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("region is required: set --region or $%s / $%s", envRegionPrimary, envRegionSecondary)
}

// firstNonEmpty は最初の空でない文字列を返す。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
