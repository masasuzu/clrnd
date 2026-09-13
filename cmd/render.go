package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	renderTfstate []string
	renderOutput  string
)

var renderCmd = &cobra.Command{
	Use:   "render [manifest]",
	Short: "Render the manifest with templates expanded",
	Long: "Render the manifest as a Go template ({{ tfstate }}, {{ env }}, ...) and print the\n" +
		"result without parsing or validating it. Useful for debugging template output.\n" +
		"This does not access the Cloud Run API and needs no --project/--region.\n" +
		"render does not check the service name, so it takes only the manifest (no service).\n" +
		"manifest may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(1),
	RunE: runRender,
}

func init() {
	addManifestFlags(renderCmd, &renderTfstate)
	renderCmd.Flags().StringVarP(&renderOutput, "output", "o", "", "output file (stdout if not set)")
}

func runRender(cmd *cobra.Command, args []string) error {
	// render does not check the name match, so it takes no service and treats its only positional
	// argument as the manifest.
	manifestPath, err := resolveManifestAt(args, 0)
	if err != nil {
		return err
	}
	ctx := cmd.Context()

	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to read manifest %s: %w", manifestPath, err)
	}
	// Print the expanded text as-is (no parsing/normalization). There is room to add --normalize
	// in the future.
	rendered, err := renderManifest(ctx, manifest, renderTfstate)
	if err != nil {
		return err
	}

	if renderOutput != "" {
		// Passing the input file itself to -o would overwrite the render source with its result.
		// Overwriting is ordinary use of render and is not forbidden, but this one case is refused.
		if sameFile(manifestPath, renderOutput) {
			return fmt.Errorf("refusing to write over the manifest being rendered: %s", renderOutput)
		}
		// The expanded content can contain secrets (via must_env, etc.), so keep it unreadable by
		// other users. An existing 0644 destination becomes 0600, and if the write fails the
		// previous content remains.
		return writeFilePrivate(renderOutput, rendered)
	}

	fmt.Fprint(cmd.OutOrStdout(), string(rendered))
	return nil
}

// sameFile reports whether two paths refer to the same file. It uses os.SameFile so that paths
// reached through a symbolic link or a hard link are still recognised as the same file.
func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}
