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
	addTargetFlags(deployCmd, &deployProject, &deployRegion)
	deployCmd.Flags().BoolVar(&deployDryRun, "dry-run", false, "validate the request server-side without applying changes")
}

func runDeploy(cmd *cobra.Command, args []string) error {
	service, manifestPath := args[0], args[1]

	project, err := resolveProject(deployProject)
	if err != nil {
		return err
	}
	region, err := resolveRegion(deployRegion)
	if err != nil {
		return err
	}
	ctx := context.Background()

	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to read manifest %s: %w", manifestPath, err)
	}

	if _, err := cloudrun.Deploy(ctx, project, region, service, manifest, deployDryRun); err != nil {
		return err
	}
	return nil
}
