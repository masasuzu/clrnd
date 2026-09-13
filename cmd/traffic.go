package cmd

import (
	"fmt"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

var (
	trafficProject  string
	trafficRegion   string
	trafficRevision string
	trafficLatest   bool
	trafficPercent  int64
	trafficApply    applyOptions
)

var trafficCmd = &cobra.Command{
	Use:   "traffic [service]",
	Short: "Change how traffic is split between revisions",
	Long: "Send a share of the traffic to a revision. Only spec.traffic changes: no new revision\n" +
		"is created, and traffic tags are kept (pinned at 0%).\n" +
		"--percent below 100 leaves the rest on the revision that is currently serving the most,\n" +
		"which is the canary shape (new 10% / stable 90%).\n" +
		"--to-latest follows whatever the newest revision is, so it is also how you undo a\n" +
		"rollback: the traffic stops being pinned to the older revision.\n" +
		"The diff is shown and confirmed the same way deploy does, and the rollout is waited\n" +
		"for unless --no-wait is given.\n" +
		"service may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(1),
	RunE: runTraffic,
}

func init() {
	addTargetFlags(trafficCmd, &trafficProject, &trafficRegion)
	addApplyFlags(trafficCmd, &trafficApply)
	trafficCmd.Flags().StringVar(&trafficRevision, "to", "",
		"revision to send traffic to")
	trafficCmd.Flags().BoolVar(&trafficLatest, "to-latest", false,
		"send traffic to the latest revision, and keep following it")
	trafficCmd.Flags().Int64Var(&trafficPercent, "percent", 100,
		"share of the traffic to send (1-100); the rest stays on the revision serving the most")
}

func runTraffic(cmd *cobra.Command, args []string) error {
	service, err := resolveService(args)
	if err != nil {
		return err
	}
	// Reject a bad combination of arguments before creating the client (= ADC discovery). As in
	// the other commands, this keeps a flag mistake from hiding behind an authentication error.
	req := cloudrun.TrafficRequest{
		Revision: trafficRevision,
		Latest:   trafficLatest,
		Percent:  trafficPercent,
	}
	if err := cloudrun.ValidateTrafficRequest(req); err != nil {
		return err
	}

	ctx := cmd.Context()
	client, err := newCloudRunClient(cmd, trafficProject, trafficRegion)
	if err != nil {
		return err
	}

	// When a name is given, confirm that the revision belongs to this service. Applying a typo as
	// "100% to a revision that does not exist" would leave the service unable to serve.
	if req.Revision != "" {
		revisions, err := client.ListRevisions(ctx, service)
		if err != nil {
			return err
		}
		target, err := cloudrun.FindRevision(revisions, req.Revision)
		if err != nil {
			return err
		}
		if !target.IsReady() {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: revision %q is not ready (%s); it may not be able to serve traffic\n",
				target.Name, readyLabelOrUnknown(target))
		}
	}

	live, err := client.GetService(ctx, service)
	if err != nil {
		return err
	}
	desired, err := cloudrun.ShiftTrafficTarget(live, req)
	if err != nil {
		return err
	}

	// desired comes from the live service, so the defaults are already filled in. No resolution is
	// needed.
	plan, err := client.PlanService(ctx, service, desired, cloudrun.PlanOptions{})
	if err != nil {
		return err
	}

	trafficApply.Prompt = fmt.Sprintf("Send %d%% of the traffic to %s?", req.Percent, trafficTargetLabel(req))
	return applyPlan(cmd, client, plan, trafficApply)
}

// trafficTargetLabel is how the destination is referred to in the confirmation prompt.
func trafficTargetLabel(req cloudrun.TrafficRequest) string {
	if req.Latest {
		return "the latest revision"
	}
	return req.Revision
}
