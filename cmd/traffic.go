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
	// 引数の組み合わせはクライアント生成 (= ADC 探索) より先に弾く。他のコマンドと同じく、
	// フラグの間違いが認証エラーの後ろに隠れないようにする。
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

	// 名前を指定した場合は、そのリビジョンがこのサービスのものか確かめる。打ち間違いを
	// 「存在しないリビジョンへ 100%」として適用すると、サービスが配信不能になる。
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

	// desired は live 由来なので既定値は既に入っている。解決は要らない。
	plan, err := client.PlanService(ctx, service, desired, cloudrun.PlanOptions{})
	if err != nil {
		return err
	}

	trafficApply.Prompt = fmt.Sprintf("Send %d%% of the traffic to %s?", req.Percent, trafficTargetLabel(req))
	return applyPlan(cmd, client, plan, trafficApply)
}

// trafficTargetLabel は確認プロンプトに出す送り先の呼び方。
func trafficTargetLabel(req cloudrun.TrafficRequest) string {
	if req.Latest {
		return "the latest revision"
	}
	return req.Revision
}
