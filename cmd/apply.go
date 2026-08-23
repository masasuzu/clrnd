package cmd

import (
	"fmt"
	"time"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

// applyOptions は適用フローの共通オプション。deploy と rollback が同じ流れを使う。
type applyOptions struct {
	DryRun      bool
	AutoApprove bool
	NoWait      bool
	Timeout     time.Duration
	// Prompt は確認プロンプトの文言。
	Prompt string
}

// addApplyFlags は適用フローに共通のフラグを登録する。
func addApplyFlags(cmd *cobra.Command, o *applyOptions) {
	cmd.Flags().BoolVar(&o.DryRun, "dry-run", false,
		"validate the request server-side without applying changes")
	cmd.Flags().BoolVar(&o.AutoApprove, "auto-approve", false,
		"apply without the interactive confirmation prompt (for CI/CD)")
	cmd.Flags().BoolVar(&o.NoWait, "no-wait", false,
		"return as soon as the request is accepted, without waiting for the rollout")
	cmd.Flags().DurationVar(&o.Timeout, "timeout", defaultRolloutTimeout,
		"how long to wait for the rollout to finish")
}

// addServerDefaultsFlag は --no-server-defaults を登録する。diff と deploy で共有する。
// 既定でサーバ既定値を解決するので、フラグは「やめる」側に置く (--no-wait と同じ形)。
func addServerDefaultsFlag(cmd *cobra.Command, skip *bool) {
	cmd.Flags().BoolVar(skip, "no-server-defaults", false,
		"compare against the manifest as written, without asking Cloud Run to fill in the fields "+
			"it defaults (avoids the dry-run write, so read-only credentials are enough)")
}

// confirmAction は取り返しのつかない操作の前に確認を取る。autoApprove なら省略し、
// 確認できない環境 (非対話な stdin) では拒否する。ok が false なら中止する
// (中止メッセージは出力済み)。action はエラー文に埋める動詞。
func confirmAction(cmd *cobra.Command, autoApprove bool, action, prompt string) (bool, error) {
	if autoApprove {
		return true, nil
	}
	if !isInteractive(cmd) {
		return false, fmt.Errorf(
			"refusing to %s without confirmation: re-run with --auto-approve (no interactive terminal)", action)
	}
	ok, err := confirm(cmd.Context(), cmd, prompt)
	if err != nil {
		return false, err
	}
	if !ok {
		fmt.Fprintln(cmd.ErrOrStderr(), "Aborted.")
	}
	return ok, nil
}

// applyPlan は差分を stdout に出し、必要なら確認を取り、適用してロールアウトを待つ。
// 状態やプロンプトは stderr、stdout はデータ (差分) 専用。
func applyPlan(cmd *cobra.Command, client *cloudrun.Client, plan *cloudrun.DeployPlan, o applyOptions) error {
	ctx := cmd.Context()

	// 差分が無ければ通常は何もしない。--dry-run はサーバ側検証のために続行する。
	if plan.Diff == "" {
		fmt.Fprintln(cmd.ErrOrStderr(), "No changes.")
		if !o.DryRun {
			return nil
		}
		_, err := plan.Apply(ctx, o.DryRun)
		return err
	}
	fmt.Fprint(cmd.OutOrStdout(), plan.Diff)

	// dry-run でなければ確認する。--auto-approve でスキップ。
	if !o.DryRun {
		ok, err := confirmAction(cmd, o.AutoApprove, "apply", o.Prompt)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}

	applied, err := plan.Apply(ctx, o.DryRun)
	if err != nil {
		return err
	}
	// --dry-run はサーバ側検証だけで何も変わらないので待たない。
	if o.DryRun || o.NoWait {
		return nil
	}
	return waitForRollout(cmd, client, plan.Service, cloudrun.WaitOptions{
		Timeout:    o.Timeout,
		Generation: cloudrun.AppliedGeneration(applied),
	})
}
