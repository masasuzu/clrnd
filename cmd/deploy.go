package cmd

import (
	"fmt"
	"os"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

var (
	deployProject  string
	deployRegion   string
	deployTfstate  []string
	deployDefaults bool
	deployApply    applyOptions
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
		"without applying any changes. Pass --server-defaults to have Cloud Run fill in the fields it\n" +
		"defaults, so a minimal manifest does not show them as a difference.\n" +
		"service and manifest may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(2),
	RunE: runDeploy,
}

func init() {
	addTargetFlags(deployCmd, &deployProject, &deployRegion)
	addManifestFlags(deployCmd, &deployTfstate)
	addApplyFlags(deployCmd, &deployApply)
	addServerDefaultsFlag(deployCmd, &deployDefaults)
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

	client, err := newCloudRunClient(cmd, deployProject, deployRegion)
	if err != nil {
		return err
	}

	plan, err := client.Plan(ctx, service, manifest, cloudrun.PlanOptions{ResolveDefaults: deployDefaults})
	if err != nil {
		return err
	}

	deployApply.Prompt = "Apply these changes?"
	return applyPlan(cmd, client, plan, deployApply)
}
