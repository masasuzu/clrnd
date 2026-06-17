package cmd

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "clrnd",
	Short: "A CLI for deploying to Cloud Run",
}

// Execute はルートコマンドを実行する。
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.AddCommand(verifyCmd)
	rootCmd.AddCommand(diffCmd)
	rootCmd.AddCommand(deployCmd)
	rootCmd.AddCommand(loadCmd)
}
