package cmd

import (
	"fmt"
	"os"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

var (
	diffProject    string
	diffRegion     string
	diffTfstate    []string
	diffImages     []string
	diffNoDefaults bool
	diffExitCode   bool
)

var diffCmd = &cobra.Command{
	Use:   "diff [service] [manifest]",
	Short: "Show the diff between an existing service and a manifest",
	Long: "Fetch the live definition of the service from Cloud Run and show a unified diff\n" +
		"against the given manifest file. Both sides are normalized (read-only fields removed)\n" +
		"before comparison. Nothing is printed when there is no difference.\n" +
		"Cloud Run fills in a lot of fields on its own, so by default diff asks it to resolve them\n" +
		"first (via a dry run) and compares against that; pass --no-server-defaults to compare\n" +
		"against the manifest as written, which also avoids needing update permission.\n" +
		"Nothing is printed when there is no difference, and diff succeeds either way unless\n" +
		"--exit-code is given.\n" +
		"service and manifest may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(2),
	RunE: runDiff,
}

func init() {
	addTargetFlags(diffCmd, &diffProject, &diffRegion)
	addManifestFlags(diffCmd, &diffTfstate)
	addImageFlag(diffCmd, &diffImages)
	addServerDefaultsFlag(diffCmd, &diffNoDefaults)
	diffCmd.Flags().BoolVar(&diffExitCode, "exit-code", false,
		"exit with 2 when there is a difference (0 when there is none, 1 on error)")
}

func runDiff(cmd *cobra.Command, args []string) error {
	service, err := resolveService(args)
	if err != nil {
		return err
	}
	manifestPath, err := resolveManifest(args)
	if err != nil {
		return err
	}

	ctx := cmd.Context()

	local, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to read manifest %s: %w", manifestPath, err)
	}
	local, err = renderManifest(ctx, local, diffTfstate)
	if err != nil {
		return err
	}
	// deploy と同じ差し替えを通す。ここを外すと、diff が見せたものと deploy が
	// 適用するものが食い違う。
	local, err = cloudrun.ApplyImageOverrides(local, diffImages)
	if err != nil {
		return err
	}
	// ローカルのパースはクライアント生成 (= ADC 探索) より先に行う。マニフェストの問題が
	// 認証エラーに隠れないようにするため。Compare も同じパースをするが、純粋な処理で安い。
	if err := cloudrun.CheckSyntax(local); err != nil {
		return err
	}

	client, err := newCloudRunClient(cmd, diffProject, diffRegion)
	if err != nil {
		return err
	}

	out, err := client.CompareManifest(ctx, service, local, manifestPath,
		cloudrun.PlanOptions{ResolveDefaults: !diffNoDefaults})
	if err != nil {
		return err
	}

	fmt.Fprint(cmd.OutOrStdout(), out)
	if diffExitCode && out != "" {
		return errDiffFound
	}
	return nil
}
