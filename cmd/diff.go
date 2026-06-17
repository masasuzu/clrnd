package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

var (
	diffProject string
	diffRegion  string
	diffTfstate []string
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
	addTargetFlags(diffCmd, &diffProject, &diffRegion)
	addManifestFlags(diffCmd, &diffTfstate)
}

func runDiff(cmd *cobra.Command, args []string) error {
	service, manifestPath := args[0], args[1]

	project, err := resolveProject(diffProject)
	if err != nil {
		return err
	}
	region, err := resolveRegion(diffRegion)
	if err != nil {
		return err
	}
	ctx := context.Background()

	local, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to read manifest %s: %w", manifestPath, err)
	}
	local, err = renderManifest(ctx, local, diffTfstate)
	if err != nil {
		return err
	}
	desired, err := cloudrun.Normalize(local)
	if err != nil {
		return err
	}

	obj, err := cloudrun.GetService(ctx, project, region, service)
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
