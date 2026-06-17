package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

var verifyTfstate []string

var verifyCmd = &cobra.Command{
	Use:   "verify <service> <manifest>",
	Short: "Verify a manifest",
	Long: "Validate that the manifest file is a well-formed Cloud Run service definition and\n" +
		"contains the fields required to deploy. This is a local check; it does not access the API\n" +
		"(unless the manifest uses {{ tfstate }} placeholders backed by a remote state).\n" +
		"Nothing is printed when the manifest is valid.",
	Args: cobra.ExactArgs(2),
	RunE: runVerify,
}

func init() {
	addManifestFlags(verifyCmd, &verifyTfstate)
}

func runVerify(cmd *cobra.Command, args []string) error {
	service, manifestPath := args[0], args[1]
	ctx := context.Background()

	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to read manifest %s: %w", manifestPath, err)
	}
	manifest, err = renderManifest(ctx, manifest, verifyTfstate)
	if err != nil {
		return err
	}

	return cloudrun.Validate(manifest, service)
}
