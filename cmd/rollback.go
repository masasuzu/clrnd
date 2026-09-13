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
	// An explicit choice of a revision that is not Ready is not blocked (the user's judgement is
	// respected), but it does not proceed silently either. If it really does not work, the wait
	// for the rollout fails it.
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

	// desired comes from the live service, so the defaults are already filled in. No resolution is
	// needed.
	plan, err := client.PlanService(ctx, service, desired, cloudrun.PlanOptions{})
	if err != nil {
		return err
	}

	rollbackApply.Prompt = fmt.Sprintf("Send all traffic to %s?", target.Name)
	return applyPlan(cmd, client, plan, rollbackApply)
}

// readyLabelOrUnknown is the Ready state shown in the warning, or "unknown" when there is no
// condition.
func readyLabelOrUnknown(r *cloudrun.Revision) string {
	if r.Ready == "" {
		return "unknown"
	}
	if r.Reason == "" {
		return r.Ready
	}
	return fmt.Sprintf("%s: %s", r.Ready, r.Reason)
}
