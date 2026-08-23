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
	// render は名前一致を検証しないので service を取らず、唯一の位置引数を manifest として扱う。
	manifestPath, err := resolveManifestAt(args, 0)
	if err != nil {
		return err
	}
	ctx := cmd.Context()

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
		// -o に入力と同じファイルを渡すと、レンダリング元が結果で潰れる。
		// 上書き自体は render の通常の使い方なので禁止しないが、これだけは断る。
		if sameFile(manifestPath, renderOutput) {
			return fmt.Errorf("refusing to write over the manifest being rendered: %s", renderOutput)
		}
		// 展開後の内容は must_env などで秘密を含みうるので、他ユーザから読めないようにする。
		if err := os.WriteFile(renderOutput, rendered, 0o600); err != nil {
			return fmt.Errorf("failed to write to %s: %w", renderOutput, err)
		}
		return nil
	}

	fmt.Fprint(cmd.OutOrStdout(), string(rendered))
	return nil
}

// sameFile は 2 つのパスが同じファイルを指すかを返す。シンボリックリンクや
// ハードリンク越しでも同一と判定できるよう os.SameFile を使う。
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
