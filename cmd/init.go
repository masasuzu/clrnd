package cmd

import (
	"context"
	"fmt"
	"os"

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
	project, err := resolveProject(initProject)
	if err != nil {
		return err
	}
	region, err := resolveRegion(initRegion)
	if err != nil {
		return err
	}
	ctx := context.Background()

	// 上書き事故を防ぐため、書き込み前に既存ファイルをまとめて確認する。
	manifestExisted := fileExists(initManifest)
	if !initForce {
		for _, path := range []string{initManifest, initConfigFile} {
			if fileExists(path) {
				return fmt.Errorf("%s already exists: pass --force to overwrite", path)
			}
		}
	}

	obj, err := cloudrun.GetService(ctx, project, region, service)
	if err != nil {
		return err
	}
	manifest, err := cloudrun.ToManifest(obj)
	if err != nil {
		return err
	}

	configYAML, err := scaffoldConfig(project, region, service, initManifest)
	if err != nil {
		return err
	}

	if err := os.WriteFile(initManifest, manifest, 0o644); err != nil {
		return fmt.Errorf("failed to write %s: %w", initManifest, err)
	}
	if err := os.WriteFile(initConfigFile, configYAML, 0o644); err != nil {
		// clrnd.yml の書き込みに失敗したら、今回新規作成した manifest を巻き戻して
		// 中途半端な scaffold を残さない (元から在ったファイルには触れない)。
		if !manifestExisted {
			os.Remove(initManifest)
		}
		return fmt.Errorf("failed to write %s: %w", initConfigFile, err)
	}
	return nil
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
