package cmd

import (
	"fmt"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// rollbackOptions holds the flags of one rollback command.
type rollbackOptions struct {
	project, region, revision string
	apply                     applyOptions
}

var rollbackCmd = newRollbackCmd(serviceTarget)

// newRollbackCmd builds rollback for a target that has revisions.
func newRollbackCmd(t target) *cobra.Command {
	o := &rollbackOptions{}
	cmd := &cobra.Command{
		Use:   fmt.Sprintf("rollback [%s]", t.arg()),
		Short: t.split.rollbackShort,
		Long: fmt.Sprintf("%[1]s\n"+
			"Without --revision, the revision just before the newest one %[2]s is chosen.\n"+
			"Only the split changes: no new revision is created%[3]s.\n"+
			"The diff is shown and confirmed the same way deploy does, and the rollout is waited\n"+
			"for unless --no-wait is given.\n"+
			"%[4]s may be omitted when set in the config file.",
			t.split.rollbackLong, t.split.serving, t.split.rollbackKeeps, t.arg()),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runRollback(cmd, args, t, o) },
	}
	addTargetFlags(cmd, &o.project, &o.region)
	addApplyFlags(cmd, &o.apply, "the rollout")
	cmd.Flags().StringVar(&o.revision, "revision", "",
		fmt.Sprintf("revision to return to (default: the one before the newest revision %s); "+
			"--to is the same flag", t.split.serving))
	// --to is the same flag: traffic calls the revision to send traffic to --to, and rollback is
	// the same move, so --to is what someone who has used traffic will try.
	cmd.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		if name == "to" {
			name = "revision"
		}
		return pflag.NormalizedName(name)
	})
	return cmd
}

func runRollback(cmd *cobra.Command, args []string, t target, o *rollbackOptions) error {
	name, err := t.resolveName(args)
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	if err := o.apply.validate(); err != nil {
		return err
	}
	client, err := newCloudRunClient(cmd, o.project, o.region)
	if err != nil {
		return err
	}

	// The resource is read once: its revisions are listed from it, and the rollback is planned
	// against the same read it edits.
	plan, chosen, err := client.PlanRollback(ctx, t.kind, name, o.revision)
	if err != nil {
		return err
	}
	// An explicit choice of a revision that is not Ready is not blocked (the user's judgement is
	// respected), but it does not proceed silently either. If it really does not work, the wait
	// for the rollout fails it.
	if !chosen.IsReady() {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: revision %q is not ready (%s); it may not be able to %s\n",
			chosen.Name, readyLabelOrUnknown(chosen), t.split.serve)
	}

	o.apply.Prompt = fmt.Sprintf(t.split.rollbackPrompt, chosen.Name)
	return applyPlan(cmd, plan, o.apply)
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
