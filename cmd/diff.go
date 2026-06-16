package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/masasuzu/crner/internal/cloudrun"
	"github.com/spf13/cobra"
)

var (
	diffProject string
	diffRegion  string
)

var diffCmd = &cobra.Command{
	Use:   "diff <service> <manifest>",
	Short: "Show the diff between an existing service and a manifest",
	Long: "Fetch the live definition of the service from Cloud Run and show a unified diff\n" +
		"against the given manifest file. Both sides are normalized (read-only fields removed)\n" +
		"before comparison. Nothing is printed when there is no difference.",
	Args: cobra.ExactArgs(2),
	RunE: runDiff,
}

func init() {
	diffCmd.Flags().StringVar(&diffProject, "project", "", "GCP project ID")
	diffCmd.Flags().StringVar(&diffRegion, "region", "", "Cloud Run region (e.g. asia-northeast1)")
	_ = diffCmd.MarkFlagRequired("project")
	_ = diffCmd.MarkFlagRequired("region")
}

func runDiff(cmd *cobra.Command, args []string) error {
	service, manifestPath := args[0], args[1]
	ctx := context.Background()

	local, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to read manifest %s: %w", manifestPath, err)
	}
	desired, err := cloudrun.Normalize(local)
	if err != nil {
		return err
	}

	obj, err := cloudrun.GetService(ctx, diffProject, diffRegion, service)
	if err != nil {
		return err
	}
	current, err := cloudrun.ToManifest(obj)
	if err != nil {
		return err
	}

	out, err := cloudrun.Diff(current, desired, "live/"+service, manifestPath)
	if err != nil {
		return err
	}

	fmt.Fprint(cmd.OutOrStdout(), out)
	return nil
}
