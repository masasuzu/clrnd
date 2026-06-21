package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	renderTfstate []string
	renderOutput  string
)

var renderCmd = &cobra.Command{
	Use:   "render [service] [manifest]",
	Short: "Render the manifest with templates expanded",
	Long: "Render the manifest as a Go template ({{ tfstate }}, {{ env }}, ...) and print the\n" +
		"result without parsing or validating it. Useful for debugging template output.\n" +
		"This does not access the Cloud Run API and needs no --project/--region.\n" +
		"manifest may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(2),
	RunE: runRender,
}

func init() {
	addManifestFlags(renderCmd, &renderTfstate)
	renderCmd.Flags().StringVarP(&renderOutput, "output", "o", "", "output file (stdout if not set)")
}

func runRender(cmd *cobra.Command, args []string) error {
	// render は名前一致を検証しないので service は解決しない (引数は manifest 解決にだけ使う)。
	manifestPath, err := resolveManifest(args)
	if err != nil {
		return err
	}
	ctx := context.Background()

	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to read manifest %s: %w", manifestPath, err)
	}
	// 展開済みテキストをそのまま出す (パース/正規化はしない)。将来 --normalize を足す余地あり。
	rendered, err := renderManifest(ctx, manifest, renderTfstate)
	if err != nil {
		return err
	}

	if renderOutput != "" {
		if err := os.WriteFile(renderOutput, rendered, 0o644); err != nil {
			return fmt.Errorf("failed to write to %s: %w", renderOutput, err)
		}
		return nil
	}

	fmt.Fprint(cmd.OutOrStdout(), string(rendered))
	return nil
}
