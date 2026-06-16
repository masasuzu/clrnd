package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/masasuzu/crner/internal/cloudrun"
	"github.com/spf13/cobra"
)

var (
	loadProject string
	loadRegion  string
	loadOutput  string
)

var loadCmd = &cobra.Command{
	Use:   "load <service>",
	Short: "Load the manifest of an existing service",
	Long: "Access the Cloud Run Admin API using the local Application Default\n" +
		"Credentials and fetch the manifest (Knative-style YAML) of the given service.",
	Args: cobra.ExactArgs(1),
	RunE: runLoad,
}

func init() {
	loadCmd.Flags().StringVar(&loadProject, "project", "", "GCP project ID")
	loadCmd.Flags().StringVar(&loadRegion, "region", "", "Cloud Run region (e.g. asia-northeast1)")
	loadCmd.Flags().StringVarP(&loadOutput, "output", "o", "", "output file (stdout if not set)")
	_ = loadCmd.MarkFlagRequired("project")
	_ = loadCmd.MarkFlagRequired("region")
}

func runLoad(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	obj, err := cloudrun.GetService(ctx, loadProject, loadRegion, args[0])
	if err != nil {
		return err
	}

	manifest, err := cloudrun.ToManifest(obj)
	if err != nil {
		return err
	}

	if loadOutput != "" {
		if err := os.WriteFile(loadOutput, manifest, 0o644); err != nil {
			return fmt.Errorf("failed to write to %s: %w", loadOutput, err)
		}
		return nil
	}

	fmt.Fprint(cmd.OutOrStdout(), string(manifest))
	return nil
}
