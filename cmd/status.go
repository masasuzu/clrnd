package cmd

import (
	"github.com/spf13/cobra"
)

var (
	statusProject string
	statusRegion  string
	statusFormat  string
)

var statusCmd = &cobra.Command{
	Use:   "status [service]",
	Short: "Show the current status of a service",
	Long: "Fetch the service from Cloud Run and print its current state: the Ready condition,\n" +
		"the latest ready and created revisions, the observed generation, the traffic split,\n" +
		"the URL, and every status condition. This is read-only; nothing is modified.\n" +
		"service may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(1),
	RunE: runStatus,
}

func init() {
	addTargetFlags(statusCmd, &statusProject, &statusRegion)
	addFormatFlag(statusCmd, &statusFormat)
}

func runStatus(cmd *cobra.Command, args []string) error {
	service, err := resolveService(args)
	if err != nil {
		return err
	}
	// フラグの検証はクライアント生成 (= ADC 探索) より先に行う。
	if err := validateFormat(statusFormat); err != nil {
		return err
	}

	ctx := cmd.Context()
	client, err := newCloudRunClient(cmd, statusProject, statusRegion)
	if err != nil {
		return err
	}

	status, err := client.Status(ctx, service)
	if err != nil {
		return err
	}

	return writeFormatted(cmd, statusFormat, status, status.Text())
}
