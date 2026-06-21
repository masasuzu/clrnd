package cmd

import (
	"os"
	"path/filepath"

	"github.com/masasuzu/clrnd/internal/config"
	"github.com/spf13/cobra"
)

// 未指定時に探す設定ファイル名 (カレントディレクトリ)。
var defaultConfigFiles = []string{"clrnd.yml", "clrnd.yaml"}

var (
	configPath string
	// cfg は読み込んだ設定。未指定なら空 (nil セーフ)。
	cfg = &config.Config{}
	// configDir は読み込んだ設定ファイルのディレクトリ。config 由来の相対パスの基準。
	configDir string
)

var rootCmd = &cobra.Command{
	Use:               "clrnd",
	Short:             "A CLI for deploying to Cloud Run",
	PersistentPreRunE: loadConfig,
}

// Execute はルートコマンドを実行する。
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", "",
		"config file (default: clrnd.yml or clrnd.yaml in the current directory)")
	rootCmd.AddCommand(verifyCmd)
	rootCmd.AddCommand(renderCmd)
	rootCmd.AddCommand(diffCmd)
	rootCmd.AddCommand(deployCmd)
	rootCmd.AddCommand(initCmd)
}

// loadConfig は --config か、未指定ならデフォルト名の設定ファイルを読み込む。
// --config 明示時にファイルが無ければエラー。自動検出時は無ければ何もしない。
func loadConfig(cmd *cobra.Command, args []string) error {
	path := configPath
	if path == "" {
		path = findDefaultConfig()
		if path == "" {
			return nil
		}
	}
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	cfg = c
	configDir = filepath.Dir(path)
	return nil
}

func findDefaultConfig() string {
	for _, name := range defaultConfigFiles {
		if info, err := os.Stat(name); err == nil && !info.IsDir() {
			return name
		}
	}
	return ""
}
