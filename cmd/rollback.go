package cmd

import (
	"fmt"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

var (
	rollbackProject  string
	rollbackRegion   string
	rollbackRevision string
	rollbackApply    applyOptions
)

var rollbackCmd = &cobra.Command{
	Use:   "rollback [service]",
	Short: "Send traffic back to an earlier revision",
	Long: "Send all traffic back to an earlier revision. Without --revision, the revision just\n" +
		"before the one currently serving is chosen. Only the traffic split changes: no new\n" +
		"revision is created, and traffic tags are kept (pinned at 0%).\n" +
		"The diff is shown and confirmed the same way deploy does, and the rollout is waited\n" +
		"for unless --no-wait is given.\n" +
		"service may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(1),
	RunE: runRollback,
}

func init() {
	addTargetFlags(rollbackCmd, &rollbackProject, &rollbackRegion)
	addApplyFlags(rollbackCmd, &rollbackApply)
	rollbackCmd.Flags().StringVar(&rollbackRevision, "revision", "",
		"revision to send traffic to (default: the one before the revision currently serving)")
}

func runRollback(cmd *cobra.Command, args []string) error {
	service, err := resolveService(args)
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	client, err := newCloudRunClient(cmd, rollbackProject, rollbackRegion)
	if err != nil {
		return err
	}

	revisions, err := client.ListRevisions(ctx, service)
	if err != nil {
		return err
	}
	target, err := cloudrun.SelectRollbackRevision(revisions, rollbackRevision)
	if err != nil {
		return err
	}
	// Ready でないリビジョンへの明示指定は止めない (利用者の判断を尊重する) が、
	// 黙って進めもしない。本当に動かないならロールアウトの待機が失敗させる。
	if !target.IsReady() {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: revision %q is not ready (%s); it may not be able to serve traffic\n",
			target.Name, readyLabelOrUnknown(target))
	}

	live, err := client.GetService(ctx, service)
	if err != nil {
		return err
	}
	desired, err := cloudrun.RollbackTarget(live, target.Name)
	if err != nil {
		return err
	}

	plan, err := client.PlanService(ctx, service, desired)
	if err != nil {
		return err
	}

	rollbackApply.Prompt = fmt.Sprintf("Send all traffic to %s?", target.Name)
	return applyPlan(cmd, client, plan, rollbackApply)
}

// readyLabelOrUnknown は警告に載せる Ready の状態。条件が無ければ "unknown"。
func readyLabelOrUnknown(r *cloudrun.Revision) string {
	if r.Ready == "" {
		return "unknown"
	}
	if r.Reason == "" {
		return r.Ready
	}
	return fmt.Sprintf("%s: %s", r.Ready, r.Reason)
}
