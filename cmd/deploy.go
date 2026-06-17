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
	deployTfstate []string
)

var deployCmd = &cobra.Command{
	Use:   "deploy [service] [manifest]",
	Short: "Deploy a manifest to Cloud Run",
	Long: "Apply the manifest to Cloud Run, creating the service if it does not exist or\n" +
		"replacing it otherwise. The manifest is validated locally before the request is sent.\n" +
		"Use --dry-run to validate server-side without applying any changes.\n" +
		"service and manifest may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(2),
	RunE: runDeploy,
}

func init() {
	addTargetFlags(deployCmd, &deployProject, &deployRegion)
	addManifestFlags(deployCmd, &deployTfstate)
	deployCmd.Flags().BoolVar(&deployDryRun, "dry-run", false, "validate the request server-side without applying changes")
}

func runDeploy(cmd *cobra.Command, args []string) error {
	service, err := resolveService(args)
	if err != nil {
		return err
	}
	manifestPath, err := resolveManifest(args)
	if err != nil {
		return err
	}

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
	manifest, err = renderManifest(ctx, manifest, deployTfstate)
	if err != nil {
		return err
	}

	if _, err := cloudrun.Deploy(ctx, project, region, service, manifest, deployDryRun); err != nil {
		return err
	}
	return nil
}
