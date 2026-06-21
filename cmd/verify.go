package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

var (
	verifyProject   string
	verifyRegion    string
	verifyTfstate   []string
	verifyLocalOnly bool
)

var verifyCmd = &cobra.Command{
	Use:   "verify [service] [manifest]",
	Short: "Verify a manifest",
	Long: "Validate that the manifest file is a well-formed Cloud Run service definition and\n" +
		"contains the fields required to deploy. This local check never needs the API.\n" +
		"When --project/--region are resolvable (and --local-only is not set), it also checks\n" +
		"via the API that referenced resources (service account, secrets) actually exist.\n" +
		"Nothing is printed when the manifest is valid.\n" +
		"service and manifest may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(2),
	RunE: runVerify,
}

func init() {
	addTargetFlags(verifyCmd, &verifyProject, &verifyRegion)
	addManifestFlags(verifyCmd, &verifyTfstate)
	verifyCmd.Flags().BoolVar(&verifyLocalOnly, "local-only", false,
		"skip the API existence checks and validate the manifest locally only")
}

func runVerify(cmd *cobra.Command, args []string) error {
	service, err := resolveService(args)
	if err != nil {
		return err
	}
	manifestPath, err := resolveManifest(args)
	if err != nil {
		return err
	}
	ctx := context.Background()

	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to read manifest %s: %w", manifestPath, err)
	}
	manifest, err = renderManifest(ctx, manifest, verifyTfstate)
	if err != nil {
		return err
	}

	// ローカルなスキーマ検証は常に行う。
	if err := cloudrun.Validate(manifest, service); err != nil {
		return err
	}

	if verifyLocalOnly {
		return nil
	}
	// project/region が解決できる場合のみ API 実在チェックを行う (CI でのオフライン
	// 検証を壊さないため、解決できなければローカル検証だけで成功とする)。
	project, region, ok := resolveTargetOptional(verifyProject, verifyRegion)
	if !ok {
		// 片方だけ明示的に指定された場合は、リモートチェックを黙ってスキップせず知らせる。
		if cmd.Flags().Changed("project") || cmd.Flags().Changed("region") {
			fmt.Fprintln(cmd.ErrOrStderr(),
				"warning: skipping API existence checks: both --project and --region must be set")
		}
		return nil
	}

	res, err := cloudrun.VerifyRemote(ctx, project, region, manifest)
	if err != nil {
		return err
	}
	// 確認できなかったもの (権限不足・API 未到達など) は警告に留め、verify は失敗させない。
	for _, u := range res.Unchecked {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not verify %s\n", u)
	}
	// 実在しないと確定したものだけを失敗として返す。
	if len(res.Missing) > 0 {
		return fmt.Errorf("%s", strings.Join(res.Missing, "\n"))
	}
	return nil
}
