package cmd

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/masasuzu/clrnd/internal/render"
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

// tfstateName は --tfstate の "name=location" 形式で name として認める文字列。
var tfstateName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// addManifestFlags は --tfstate フラグを登録する。繰り返し指定可能。
func addManifestFlags(cmd *cobra.Command, tfstate *[]string) {
	cmd.Flags().StringArrayVar(tfstate, "tfstate", nil,
		"Terraform state for {{ tfstate }} placeholders: <location> or <name>=<location> "+
			"(repeatable; local path or s3://, gs://, ... URL)")
}

// renderManifest は --tfstate 指定を解釈し、マニフェストのプレースホルダーを埋める。
func renderManifest(ctx context.Context, manifest []byte, tfstateSpecs []string) ([]byte, error) {
	sources, err := parseTfstateSources(tfstateSpecs)
	if err != nil {
		return nil, err
	}
	return render.Render(ctx, manifest, sources)
}

// parseTfstateSources は --tfstate の各指定を render.Source に変換する。
// "name=location" は名前付き、"location" のみは "default" として扱う。
// location に "=" を含む URL もあるため、name は先頭の "=" より前が name 形式の
// 場合に限り採用する。
func parseTfstateSources(specs []string) ([]render.Source, error) {
	var out []render.Source
	seen := make(map[string]bool)
	for _, spec := range specs {
		name, loc := "default", spec
		if i := strings.Index(spec, "="); i > 0 && tfstateName.MatchString(spec[:i]) {
			name, loc = spec[:i], spec[i+1:]
		}
		if loc == "" {
			return nil, fmt.Errorf("invalid --tfstate %q: location is empty", spec)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate --tfstate name %q", name)
		}
		seen[name] = true
		out = append(out, render.Source{Name: name, Location: loc})
	}
	return out, nil
}

// firstNonEmpty は前後の空白を除いて最初の空でない文字列を (トリム済みで) 返す。
// 空白のみの値は未設定として扱い、次のソースへフォールバックする。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
