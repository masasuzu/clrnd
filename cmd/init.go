package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
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
	if !initForce {
		for _, path := range []string{initManifest, initConfigFile} {
			if _, err := os.Stat(path); err == nil {
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

	if err := os.WriteFile(initManifest, manifest, 0o644); err != nil {
		return fmt.Errorf("failed to write %s: %w", initManifest, err)
	}
	if err := os.WriteFile(initConfigFile, scaffoldConfig(project, region, service, initManifest), 0o644); err != nil {
		return fmt.Errorf("failed to write %s: %w", initConfigFile, err)
	}
	return nil
}

// scaffoldConfig は init が生成する clrnd.yml の中身を組み立てる。
func scaffoldConfig(project, region, service, manifest string) []byte {
	return []byte(fmt.Sprintf("project: %s\nregion: %s\nservice: %s\nmanifest: %s\n",
		project, region, service, manifest))
}
