package cmd

import (
	"fmt"
	"os"

	"github.com/masasuzu/crner/internal/cloudrun"
	"github.com/spf13/cobra"
)

var verifyCmd = &cobra.Command{
	Use:   "verify <service> <manifest>",
	Short: "Verify a manifest",
	Long: "Validate that the manifest file is a well-formed Cloud Run service definition and\n" +
		"contains the fields required to deploy. This is a local check; it does not access the API.\n" +
		"Nothing is printed when the manifest is valid.",
	Args: cobra.ExactArgs(2),
	RunE: runVerify,
}

func runVerify(cmd *cobra.Command, args []string) error {
	service, manifestPath := args[0], args[1]

	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to read manifest %s: %w", manifestPath, err)
	}

	return cloudrun.Validate(manifest, service)
}
