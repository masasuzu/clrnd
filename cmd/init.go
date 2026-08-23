package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/masasuzu/clrnd/internal/config"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

// init が生成する設定ファイル名。ルートの自動検出名 (defaultConfigFiles) の先頭に合わせる。
const initConfigFile = "clrnd.yml"

var (
	initProject  string
	initRegion   string
	initManifest string
	initForce    bool
)

var initCmd = &cobra.Command{
	Use:     "init [service]",
	Aliases: []string{"load"},
	Short:   "Initialize a project from an existing service",
	Long: "Fetch an existing Cloud Run service and scaffold a project from it: write its\n" +
		"manifest (Knative-style YAML, with server-managed fields stripped) and a clrnd.yml\n" +
		"holding the project, region, service, and manifest path. Existing files are not\n" +
		"overwritten unless --force is given.\n" +
		"service may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(1),
	RunE: runInit,
	// 生成先を -c で指定できるようにする。生成するファイルなので、まだ無くてもよい。
	Annotations: map[string]string{annotationConfigOptional: ""},
}

func init() {
	addTargetFlags(initCmd, &initProject, &initRegion)
	initCmd.Flags().StringVarP(&initManifest, "output", "o", "manifest.yaml", "manifest file to write")
	initCmd.Flags().BoolVar(&initForce, "force", false, "overwrite existing files")
}

func runInit(cmd *cobra.Command, args []string) error {
	service, err := resolveService(args)
	if err != nil {
		return err
	}

	// --config が指定されていればそこへ書く。読む場所と書く場所が食い違うと、
	// -c infra/clrnd.yml を渡したのに ./clrnd.yml が生まれる。
	configFile := initConfigFile
	if configPath != "" {
		configFile = configPath
	}

	// 上書き事故を防ぐため、書き込み前に既存ファイルをまとめて確認する。
	manifestExisted := fileExists(initManifest)
	if !initForce {
		for _, path := range []string{initManifest, configFile} {
			if fileExists(path) {
				return fmt.Errorf("%s already exists: pass --force to overwrite", path)
			}
		}
	}

	ctx := cmd.Context()
	client, err := newCloudRunClient(cmd, initProject, initRegion)
	if err != nil {
		return err
	}

	obj, err := client.GetService(ctx, service)
	if err != nil {
		return err
	}
	// live のリビジョン名をそのまま残すと、テンプレートを変えた 2 回目の deploy が
	// 「設定の異なる同名リビジョンは作れない」で失敗する。scaffold では落として自動採番に任せる。
	manifest, err := cloudrun.ToManifest(cloudrun.WithoutRevisionName(obj))
	if err != nil {
		return err
	}

	// config に書くマニフェストのパスは config ファイルからの相対にする。
	// resolveConfigPath が config のディレクトリ基準で解決するため、cwd 基準のまま
	// 記録すると -c で別ディレクトリを指したときにパスが壊れる。
	configYAML, err := scaffoldConfig(client.Project(), client.Region(), service,
		manifestPathFor(configFile, initManifest))
	if err != nil {
		return err
	}

	// --force で既存のマニフェストを潰す場合は、巻き戻せるように中身を控えておく。
	var previousManifest []byte
	if manifestExisted {
		previousManifest, _ = os.ReadFile(initManifest)
	}

	// 生成物には live の平文の環境変数が入りうるので、他ユーザから読めないようにする。
	// --force が無い場合は作成と存在確認を 1 回の操作で行い、上の確認の後に作られた
	// ファイルを潰さない。--force の場合は原子的に置き換える。
	if err := writeScaffold(initManifest, manifest, initForce); err != nil {
		return err
	}
	if err := writeScaffold(configFile, configYAML, initForce); err != nil {
		// config の書き込みに失敗したら manifest を元に戻し、中途半端な scaffold を
		// 残さない。巻き戻しは best-effort で、返すのは本来の書き込みエラー。
		restoreManifest(initManifest, previousManifest, manifestExisted)
		return err
	}
	return nil
}

// writeScaffold は init の生成物を 1 つ書く。force なら既存ファイルを原子的に置き換え、
// そうでなければ既に在る場合に失敗する。
func writeScaffold(path string, data []byte, force bool) error {
	if force {
		return writeFilePrivate(path, data)
	}
	return writeFileExclusive(path, data)
}

// manifestPathFor は config ファイルから見たマニフェストの相対パスを返す。
// 相対にできない場合 (別ボリューム等) は与えられたパスをそのまま使う。
func manifestPathFor(configFile, manifest string) string {
	rel, err := filepath.Rel(filepath.Dir(configFile), manifest)
	if err != nil {
		return manifest
	}
	return rel
}

// restoreManifest は init が書き換えたマニフェストを元の状態に戻す。
// 元から無かった場合は削除し、在った場合は控えておいた中身を書き戻す。
func restoreManifest(path string, previous []byte, existed bool) {
	if !existed {
		_ = os.Remove(path)
		return
	}
	if previous != nil {
		_ = writeFilePrivate(path, previous)
	}
}

// fileExists は path にファイル (またはディレクトリ) が存在するかを返す。
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// scaffoldConfig は init が生成する clrnd.yml の中身を組み立てる。手書きせず config.Config を
// マーシャルすることで、値のエスケープ (パスにコロン等が含まれる場合) を YAML 側に任せ、
// clrnd.yml を読む側 (config.Load) とスキーマがずれないようにする。
func scaffoldConfig(project, region, service, manifest string) ([]byte, error) {
	out, err := yaml.Marshal(config.Config{
		Project:  project,
		Region:   region,
		Service:  service,
		Manifest: manifest,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to build %s: %w", initConfigFile, err)
	}
	return out, nil
}
