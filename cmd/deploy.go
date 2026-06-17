package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

var (
	deployProject string
	deployRegion  string
	deployDryRun  bool
)

var deployCmd = &cobra.Command{
	Use:   "deploy <service> <manifest>",
	Short: "Deploy a manifest to Cloud Run",
	Long: "Apply the manifest to Cloud Run, creating the service if it does not exist or\n" +
		"replacing it otherwise. The manifest is validated locally before the request is sent.\n" +
		"Use --dry-run to validate server-side without applying any changes.",
	Args: cobra.ExactArgs(2),
	RunE: runDeploy,
}

func init() {
	deployCmd.Flags().StringVar(&deployProject, "project", "", "GCP project ID")
	deployCmd.Flags().StringVar(&deployRegion, "region", "", "Cloud Run region (e.g. asia-northeast1)")
	deployCmd.Flags().BoolVar(&deployDryRun, "dry-run", false, "validate the request server-side without applying changes")
	_ = deployCmd.MarkFlagRequired("project")
	_ = deployCmd.MarkFlagRequired("region")
}

func runDeploy(cmd *cobra.Command, args []string) error {
	service, manifestPath := args[0], args[1]
	ctx := context.Background()

	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to read manifest %s: %w", manifestPath, err)
	}

	if _, err := cloudrun.Deploy(ctx, deployProject, deployRegion, service, manifest, deployDryRun); err != nil {
		return err
	}
	return nil
}
