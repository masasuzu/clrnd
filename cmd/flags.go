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

// resolveService は位置引数 args[0] > config service の順で解決する。
func resolveService(args []string) (string, error) {
	if len(args) >= 1 && args[0] != "" {
		return args[0], nil
	}
	if cfg.Service != "" {
		return cfg.Service, nil
	}
	return "", fmt.Errorf("service is required: pass it as an argument or set service in the config file")
}

// resolveManifest は位置引数 args[1] > config manifest の順で解決する。
func resolveManifest(args []string) (string, error) {
	if len(args) >= 2 && args[1] != "" {
		return args[1], nil
	}
	if cfg.Manifest != "" {
		return cfg.Manifest, nil
	}
	return "", fmt.Errorf("manifest is required: pass it as an argument or set manifest in the config file")
}

// resolveProject はフラグ > 環境変数 > config の順で解決する (gcloud と同じ優先順位)。
// どこにも無ければエラー。
func resolveProject(flag string) (string, error) {
	if v := firstNonEmpty(flag, os.Getenv(envProjectPrimary), os.Getenv(envProjectSecondary), cfg.Project); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("project is required: set --project, $%s / $%s, or project in the config file", envProjectPrimary, envProjectSecondary)
}

// resolveRegion はフラグ > 環境変数 > config の順で解決する。どこにも無ければエラー。
func resolveRegion(flag string) (string, error) {
	if v := firstNonEmpty(flag, os.Getenv(envRegionPrimary), os.Getenv(envRegionSecondary), cfg.Region); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("region is required: set --region, $%s / $%s, or region in the config file", envRegionPrimary, envRegionSecondary)
}

// tfstateName は --tfstate の "name=location" 形式で name として認める文字列。
var tfstateName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// addManifestFlags は --tfstate フラグを登録する。繰り返し指定可能。
func addManifestFlags(cmd *cobra.Command, tfstate *[]string) {
	cmd.Flags().StringArrayVar(tfstate, "tfstate", nil,
		"Terraform state for {{ tfstate }} placeholders: <location> or <name>=<location> "+
			"(repeatable; local path or s3://, gs://, ... URL)")
}

// renderManifest は tfstate 指定 (フラグ優先、無ければ config) を解釈し、マニフェストの
// プレースホルダーを埋める。
func renderManifest(ctx context.Context, manifest []byte, tfstateSpecs []string) ([]byte, error) {
	sources, err := resolveTfstateSources(tfstateSpecs)
	if err != nil {
		return nil, err
	}
	return render.Render(ctx, manifest, sources)
}

// resolveTfstateSources は --tfstate フラグが指定されていればそれを使い、無ければ config の
// tfstate を使う (フラグが config を置き換える)。
func resolveTfstateSources(specs []string) ([]render.Source, error) {
	if len(specs) > 0 {
		return parseTfstateSources(specs)
	}
	return configTfstateSources()
}

// configTfstateSources は config の tfstate を render.Source に変換する。
func configTfstateSources() ([]render.Source, error) {
	var out []render.Source
	seen := make(map[string]bool)
	for _, t := range cfg.Tfstate {
		name := t.Name
		if name == "" {
			name = "default"
		}
		if t.Location == "" {
			return nil, fmt.Errorf("config tfstate %q: location is required", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate tfstate name %q in config", name)
		}
		seen[name] = true
		out = append(out, render.Source{Name: name, Location: t.Location})
	}
	return out, nil
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
