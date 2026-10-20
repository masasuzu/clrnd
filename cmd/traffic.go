package cmd

import (
	"fmt"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

// splitOptions holds the flags of one command that changes a split: traffic for a service,
// instance-split for a worker pool.
type splitOptions struct {
	project, region, revision string
	latest                    bool
	percent                   int64
	apply                     applyOptions
}

var trafficCmd = newSplitCmd(serviceTarget)

// newSplitCmd builds the command that changes how a target's revisions share what it runs:
// traffic for a service, instance-split for a worker pool. The name, help and prompt come from the
// target's revisionSplit.
func newSplitCmd(t target) *cobra.Command {
	o := &splitOptions{}
	s := t.split
	cmd := &cobra.Command{
		Use:   fmt.Sprintf("%s [%s]", s.command, t.arg()),
		Short: s.commandShort,
		Long:  s.commandLong,
		Args:  cobra.MaximumNArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runSplit(cmd, args, t, o) },
	}
	addTargetFlags(cmd, &o.project, &o.region)
	addApplyFlags(cmd, &o.apply, "the rollout")
	cmd.Flags().StringVar(&o.revision, "to", "", "revision to "+s.sendTo)
	cmd.Flags().BoolVar(&o.latest, "to-latest", false, s.toLatestUsage)
	cmd.Flags().Int64Var(&o.percent, "percent", 100,
		fmt.Sprintf("share of the %s to send (1-100); the rest stays on the revision %s the most",
			s.what, s.verb))
	return cmd
}

func runSplit(cmd *cobra.Command, args []string, t target, o *splitOptions) error {
	name, err := t.resolveName(args)
	if err != nil {
		return err
	}
	// Reject a bad combination of arguments before creating the client (= ADC discovery). As in
	// the other commands, this keeps a flag mistake from hiding behind an authentication error.
	req := cloudrun.SplitRequest{
		Revision: o.revision,
		Latest:   o.latest,
		Percent:  o.percent,
	}
	if err := cloudrun.ValidateSplitRequest(t.kind, req); err != nil {
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

	// The resource is read once: a named revision is checked against its revisions (applying a
	// typo as "100% to a revision that does not exist" would leave it with nothing to run), and the
	// split is planned against the same read.
	plan, named, err := client.PlanSplit(ctx, t.kind, name, req)
	if err != nil {
		return err
	}
	if named != nil && !named.IsReady() {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: revision %q is not ready (%s); it may not be able to %s\n",
			named.Name, readyLabelOrUnknown(named), t.split.serve)
	}

	o.apply.Prompt = fmt.Sprintf(t.split.splitPrompt, req.Percent, splitTargetLabel(req))
	return applyPlan(cmd, plan, o.apply)
}

// splitTargetLabel is how the destination is referred to in the confirmation prompt.
func splitTargetLabel(req cloudrun.SplitRequest) string {
	if req.Latest {
		return "the latest revision"
	}
	return req.Revision
}
