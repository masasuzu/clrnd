package cmd

import (
	"fmt"
	"os"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

var (
	deployProject    string
	deployRegion     string
	deployTfstate    []string
	deployNoDefaults bool
	deployNoTraffic  bool
	deployApply      applyOptions
)

var deployCmd = &cobra.Command{
	Use:   "deploy [service] [manifest]",
	Short: "Deploy a manifest to Cloud Run",
	Long: "Show the diff against the live service, then (after confirmation) apply the manifest to\n" +
		"Cloud Run, creating the service if it does not exist or replacing it otherwise. The manifest\n" +
		"is validated locally before the request is sent.\n" +
		"After applying, deploy waits until the new revision is serving and fails if the rollout\n" +
		"fails; pass --no-wait to return as soon as the request is accepted.\n" +
		"Use --auto-approve to skip the prompt (for CI/CD), or --dry-run to validate server-side\n" +
		"without applying any changes. The diff resolves the fields Cloud Run defaults first, so a\n" +
		"minimal manifest does not show them as a difference; --no-server-defaults skips that.\n" +
		"With --no-traffic the new revision is created without receiving any traffic: the current\n" +
		"split is pinned to the revisions serving it now, so you can move traffic over afterwards\n" +
		"with 'clrnd traffic'.\n" +
		"service and manifest may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(2),
	RunE: runDeploy,
}

func init() {
	addTargetFlags(deployCmd, &deployProject, &deployRegion)
	addManifestFlags(deployCmd, &deployTfstate)
	addApplyFlags(deployCmd, &deployApply)
	addServerDefaultsFlag(deployCmd, &deployNoDefaults)
	deployCmd.Flags().BoolVar(&deployNoTraffic, "no-traffic", false,
		"create the revision without sending traffic to it (keep the current split)")
}

func runDeploy(cmd *cobra.Command, args []string) error {
	service, err := resolveService(args)
	if err != nil {
		return err
	}
	manifestPath, err := resolveManifest(args)
	if err != nil {
		return err
	}

	ctx := cmd.Context()

	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to read manifest %s: %w", manifestPath, err)
	}
	manifest, err = renderManifest(ctx, manifest, deployTfstate)
	if err != nil {
		return err
	}

	// ローカル検証はクライアント生成 (= ADC 探索) と project/region 解決より先に行う。
	// 認証情報や target が無い環境でも、マニフェストの問題がそれらのエラーに隠れない
	// ようにするため。Plan も内部で同じ検証をするが、純粋な処理なので二重でも安い。
	if err := cloudrun.Validate(manifest, service); err != nil {
		return err
	}
	// deploy だけを回す CI でも、リビジョン名固定が原因の 409 を事前に説明できるようにする。
	if err := warnPinnedRevision(cmd, manifest); err != nil {
		return err
	}
	// --no-traffic はマニフェストの spec.traffic を無視して現在の配分で置き換える。
	// 黙って上書きすると「書いたのに効かない」になるので、書いてある場合は言う。
	// これもローカルな検査なので、クライアント生成 (= ADC 探索) より前に出す。
	if deployNoTraffic {
		if err := warnManifestTraffic(cmd, manifest); err != nil {
			return err
		}
	}

	client, err := newCloudRunClient(cmd, deployProject, deployRegion)
	if err != nil {
		return err
	}

	plan, err := client.Plan(ctx, service, manifest, cloudrun.PlanOptions{
		ResolveDefaults: !deployNoDefaults,
		KeepTraffic:     deployNoTraffic,
	})
	if err != nil {
		return err
	}

	deployApply.Prompt = "Apply these changes?"
	return applyPlan(cmd, client, plan, deployApply)
}
