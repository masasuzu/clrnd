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
	verifyImages    []string
	verifyLocalOnly bool
	verifyFormat    string
)

// verifyResult は --format json の出力。text の出力 (stderr の warning と、失敗時の
// エラー) と同じ材料を構造化したもので、CI が「警告を無視するか失敗にするか」を
// 自分で決められるようにする。
type verifyResult struct {
	Service  string `json:"service"`
	Manifest string `json:"manifest"`
	// OK は失敗が無かったか。Missing があるか、ローカル検証に失敗すると false。
	OK bool `json:"ok"`
	// Errors はローカル検証の失敗。
	Errors []string `json:"errors,omitempty"`
	// Missing は実在しないと確定した参照 (失敗)。
	Missing []string `json:"missing,omitempty"`
	// Unchecked は確認できなかった参照 (警告)。
	Unchecked []string `json:"unchecked,omitempty"`
	// Warnings はそれ以外の助言 (リビジョン名の固定など)。
	Warnings []string `json:"warnings,omitempty"`
}

var verifyCmd = &cobra.Command{
	Use:   "verify [service] [manifest]",
	Short: "Verify a manifest",
	Long: "Validate that the manifest file is a well-formed Cloud Run service definition and\n" +
		"contains the fields required to deploy. This local check never needs the API.\n" +
		"When --project/--region are resolvable (and --local-only is not set), it also checks\n" +
		"via the API that referenced resources actually exist: the service account, the Secret\n" +
		"Manager secrets and the versions they reference, the VPC connector and Cloud SQL\n" +
		"instances named in the annotations, and container images hosted on Artifact Registry\n" +
		"(*-docker.pkg.dev).\n" +
		"Images on gcr.io, Docker Hub, or any other registry are not checked and not reported.\n" +
		"--image checks the image you would deploy with, not the one written in the manifest.\n" +
		"A valid manifest produces no output on stdout; warnings (a pinned revision name, a check\n" +
		"that could not be completed) go to stderr without failing the command.\n" +
		"--format json prints the result instead — missing, unchecked and warnings as one object\n" +
		"on stdout — while the exit code stays the same.\n" +
		"service and manifest may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(2),
	RunE: runVerify,
}

func init() {
	addTargetFlags(verifyCmd, &verifyProject, &verifyRegion)
	addManifestFlags(verifyCmd, &verifyTfstate)
	addImageFlag(verifyCmd, &verifyImages)
	verifyCmd.Flags().BoolVar(&verifyLocalOnly, "local-only", false,
		"skip the API existence checks and validate the manifest locally only")
	addFormatFlag(verifyCmd, &verifyFormat)
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
	// 出力形式の検証は、他のローカルな検査と同じくクライアント生成より先に行う。
	if err := validateFormat(verifyFormat); err != nil {
		return err
	}
	ctx := cmd.Context()

	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to read manifest %s: %w", manifestPath, err)
	}
	manifest, err = renderManifest(ctx, manifest, verifyTfstate)
	if err != nil {
		return err
	}

	result := verifyResult{Service: service, Manifest: manifestPath}

	// deploy が適用するものを検証するため、同じ差し替えを通す (イメージの実在チェックも
	// 差し替え後のイメージに対して行われる)。ここから先の失敗は finishVerify を通す:
	// --format json の利用者にとって、stdout が空のまま終わる経路があると
	// `clrnd verify --format json | jq ...` が読めない出力で落ちる。
	manifest, err = cloudrun.ApplyImageOverrides(manifest, verifyImages)
	if err != nil {
		result.Errors = strings.Split(err.Error(), "\n")
		return finishVerify(cmd, result, err)
	}

	// ローカルなスキーマ検証は常に行う。Validate は errors.Join なので、1 行 1 件に
	// ばらして構造化する (text 出力は従来どおりエラーとしてまとめて出る)。
	if err := cloudrun.Validate(manifest, service); err != nil {
		result.Errors = strings.Split(err.Error(), "\n")
		return finishVerify(cmd, result, err)
	}

	// リビジョン名の固定は文法上は正しいが、次にテンプレートを変えたときの deploy が
	// 必ず失敗するので警告する (失敗にはしない: 使い捨てのデプロイでは正しい書き方)。
	warning, err := pinnedRevisionWarning(manifest)
	if err != nil {
		return err
	}
	if warning != "" {
		result.Warnings = append(result.Warnings, warning)
	}

	if !verifyLocalOnly {
		if err := verifyRemote(ctx, cmd, manifest, &result); err != nil {
			result.Errors = append(result.Errors, err.Error())
			return finishVerify(cmd, result, err)
		}
	}

	// 実在しないと確定したものだけを失敗として返す。
	var failure error
	if len(result.Missing) > 0 {
		failure = fmt.Errorf("%s", strings.Join(result.Missing, "\n"))
	}
	return finishVerify(cmd, result, failure)
}

// verifyRemote は API 実在チェックを走らせ、結果を result に足す。target が解決できない
// 場合は何もしない (CI でのオフライン検証を壊さないため)。
//
// リージョンを使うのは VPC コネクタの短縮名を完全なリソース名に補うときだけ
// (IAM / Secret Manager / Artifact Registry / Cloud SQL はリージョンを取らない) だが、
// 「デプロイ先が決まっている」条件は両方揃っている方を採る: 片方しか無い状態は設定
// ミスの可能性が高く、黙って本番のプロジェクトを引きに行くより何もしない方が安全。
func verifyRemote(ctx context.Context, cmd *cobra.Command, manifest []byte, result *verifyResult) error {
	project, region, ok := resolveTargetOptional(verifyProject, verifyRegion)
	if !ok {
		// 片方だけ明示的に指定された場合は、リモートチェックを黙ってスキップせず知らせる。
		if cmd.Flags().Changed("project") || cmd.Flags().Changed("region") {
			result.Warnings = append(result.Warnings,
				"skipping API existence checks: both --project and --region must be set")
		}
		return nil
	}

	res, err := cloudrun.VerifyRemote(ctx, project, region, manifest, clientOptions...)
	if err != nil {
		return err
	}
	result.Missing = append(result.Missing, res.Missing...)
	result.Unchecked = append(result.Unchecked, res.Unchecked...)
	return nil
}

// finishVerify は結果を出力し、失敗があればそれを返す。
//
// text は従来どおり: 成功時の stdout は空で、警告は stderr、失敗はエラーとして返す
// (cobra が stderr に出す)。json は結果を 1 つのオブジェクトとして stdout に出したうえで、
// 失敗はやはりエラーとして返す — stdout をデータ専用に保ちつつ、終了コードも変えないため。
func finishVerify(cmd *cobra.Command, result verifyResult, failure error) error {
	result.OK = failure == nil

	if verifyFormat == formatJSON {
		if err := writeFormatted(cmd, formatJSON, result, ""); err != nil {
			return err
		}
		return failure
	}

	out := cmd.ErrOrStderr()
	for _, w := range result.Warnings {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
	// 確認できなかったもの (権限不足・API 未到達など) は警告に留め、verify は失敗させない。
	for _, u := range result.Unchecked {
		fmt.Fprintf(out, "warning: could not verify %s\n", u)
	}
	return failure
}
