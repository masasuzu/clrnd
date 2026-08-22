package cmd

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

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

// Execute はルートコマンドを実行する。SIGINT/SIGTERM で cancel される context を渡すので、
// 各サブコマンドは cmd.Context() を使うことで Ctrl-C で中断できる。
func Execute() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 1 回目のシグナルで ctx を cancel した後はハンドラを解除し、既定の挙動 (即終了) に
	// 戻す。解除しないと 2 回目以降のシグナルが握り潰され、ctx を見ない処理に入っている
	// 間はプロセスを止める手段が無くなる。
	go func() {
		<-ctx.Done()
		stop()
	}()
	return rootCmd.ExecuteContext(ctx)
}

func init() {
	rootCmd.Version = buildVersion()
	rootCmd.SetVersionTemplate("{{ .Name }} version {{ .Version }}\n")
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
