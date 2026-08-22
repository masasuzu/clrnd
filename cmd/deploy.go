package cmd

import (
	"fmt"
	"os"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

var (
	deployProject     string
	deployRegion      string
	deployDryRun      bool
	deployAutoApprove bool
	deployTfstate     []string
)

var deployCmd = &cobra.Command{
	Use:   "deploy [service] [manifest]",
	Short: "Deploy a manifest to Cloud Run",
	Long: "Show the diff against the live service, then (after confirmation) apply the manifest to\n" +
		"Cloud Run, creating the service if it does not exist or replacing it otherwise. The manifest\n" +
		"is validated locally before the request is sent.\n" +
		"Use --auto-approve to skip the prompt (for CI/CD), or --dry-run to validate server-side\n" +
		"without applying any changes. service and manifest may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(2),
	RunE: runDeploy,
}

func init() {
	addTargetFlags(deployCmd, &deployProject, &deployRegion)
	addManifestFlags(deployCmd, &deployTfstate)
	deployCmd.Flags().BoolVar(&deployDryRun, "dry-run", false, "validate the request server-side without applying changes")
	deployCmd.Flags().BoolVar(&deployAutoApprove, "auto-approve", false, "apply without the interactive confirmation prompt (for CI/CD)")
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

	plan, err := client.Plan(ctx, service, manifest)
	if err != nil {
		return err
	}

	// 差分を表示する (stdout)。差分が無ければ通常は何もしないが、--dry-run は
	// サーバ側検証を行うため続行する。
	if plan.Diff == "" {
		fmt.Fprintln(cmd.ErrOrStderr(), "No changes.")
		if !deployDryRun {
			return nil
		}
		return plan.Apply(ctx, deployDryRun)
	}
	fmt.Fprint(cmd.OutOrStdout(), plan.Diff)

	// dry-run でなければ確認する。--auto-approve でスキップ。
	if !deployDryRun && !deployAutoApprove {
		if !isInteractive(cmd) {
			return fmt.Errorf("refusing to apply without confirmation: re-run with --auto-approve (no interactive terminal)")
		}
		ok, err := confirm(ctx, cmd, "Apply these changes?")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(cmd.ErrOrStderr(), "Aborted.")
			return nil
		}
	}

	return plan.Apply(ctx, deployDryRun)
}
