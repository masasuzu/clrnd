package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// status の出力形式。-o/--output は init/render が「出力ファイル」に使っているので、
// 形式の指定には gcloud と同じ --format を使う (名前が同じで意味が違う状態を作らない)。
const (
	formatText = "text"
	formatJSON = "json"
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
	statusCmd.Flags().StringVar(&statusFormat, "format", formatText,
		fmt.Sprintf("output format: %s or %s", formatText, formatJSON))
}

func runStatus(cmd *cobra.Command, args []string) error {
	service, err := resolveService(args)
	if err != nil {
		return err
	}
	// フラグの検証はクライアント生成 (= ADC 探索) より先に行う。
	if statusFormat != formatText && statusFormat != formatJSON {
		return fmt.Errorf("invalid --format %q: must be %q or %q", statusFormat, formatText, formatJSON)
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

	if statusFormat == formatJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}
	fmt.Fprint(cmd.OutOrStdout(), status.Text())
	return nil
}
