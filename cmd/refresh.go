package cmd

import (
	"fmt"
	"time"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

var (
	refreshProject string
	refreshRegion  string
	refreshSuffix  string
	refreshApply   applyOptions
)

var refreshCmd = &cobra.Command{
	Use:   "refresh [service]",
	Short: "Roll out a new revision without changing the definition",
	Long: "Re-apply the live definition of the service so that a new revision is created, without\n" +
		"changing anything about it. Useful when the image tag still points somewhere new, or to\n" +
		"restart the containers.\n" +
		"Cloud Run only creates a revision when spec.template changes, so refresh gives the new\n" +
		"revision an explicit name (<service>-r<UTC timestamp>, or --revision-suffix). That name\n" +
		"is dropped again by the next deploy from a manifest.\n" +
		"The diff is shown and confirmed the same way deploy does, and the rollout is waited for\n" +
		"unless --no-wait is given. service may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(1),
	RunE: runRefresh,
}

func init() {
	addTargetFlags(refreshCmd, &refreshProject, &refreshRegion)
	addApplyFlags(refreshCmd, &refreshApply)
	refreshCmd.Flags().StringVar(&refreshSuffix, "revision-suffix", "",
		"suffix for the new revision name (default: r<UTC timestamp>)")
}

func runRefresh(cmd *cobra.Command, args []string) error {
	service, err := resolveService(args)
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	client, err := newCloudRunClient(cmd, refreshProject, refreshRegion)
	if err != nil {
		return err
	}

	// refresh はローカルのマニフェストを見ない。いま動いている定義をそのまま流し直す。
	live, err := client.GetService(ctx, service)
	if err != nil {
		return err
	}

	suffix := refreshSuffix
	if suffix == "" {
		suffix = cloudrun.RefreshSuffix(time.Now())
	}
	desired, err := cloudrun.RefreshTarget(live, service, suffix)
	if err != nil {
		return err
	}

	// desired は live 由来なので既定値は既に入っている。解決は要らない。
	plan, err := client.PlanService(ctx, service, desired, cloudrun.PlanOptions{})
	if err != nil {
		return err
	}

	refreshApply.Prompt = fmt.Sprintf("Roll out a new revision %s-%s?", service, suffix)
	return applyPlan(cmd, client, plan, refreshApply)
}
