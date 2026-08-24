package cmd

import (
	"fmt"
	"time"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

// deploy と wait で共有するロールアウト待機の既定値。
const (
	defaultRolloutTimeout  = 10 * time.Minute
	defaultRolloutInterval = 2 * time.Second
)

var (
	waitProject  string
	waitRegion   string
	waitTimeout  time.Duration
	waitInterval time.Duration
)

var waitCmd = &cobra.Command{
	Use:   "wait [service]",
	Short: "Wait until a service is ready",
	Long: "Poll the service until its Ready condition becomes True. If it becomes False the\n" +
		"command fails immediately instead of waiting for the timeout. Progress is reported\n" +
		"on stderr; nothing is written to stdout. Ctrl-C stops the wait.\n" +
		"service may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(1),
	RunE: runWait,
}

func init() {
	addTargetFlags(waitCmd, &waitProject, &waitRegion)
	waitCmd.Flags().DurationVar(&waitTimeout, "timeout", defaultRolloutTimeout,
		"how long to wait before giving up")
	waitCmd.Flags().DurationVar(&waitInterval, "interval", defaultRolloutInterval,
		"initial polling interval (it backs off up to 15s)")
}

func runWait(cmd *cobra.Command, args []string) error {
	service, err := resolveService(args)
	if err != nil {
		return err
	}
	client, err := newCloudRunClient(cmd, waitProject, waitRegion)
	if err != nil {
		return err
	}
	return waitForRollout(cmd, client, service, cloudrun.WaitOptions{
		Timeout:  waitTimeout,
		Interval: waitInterval,
	})
}

// waitForDeletion はサービスが実際に消えるまで待つ。Cloud Run の削除は非同期なので、
// これが無いと delete の直後にまだ取得できてしまう。
func waitForDeletion(cmd *cobra.Command, client *cloudrun.Client, service string,
	timeout, interval time.Duration) error {
	out := cmd.ErrOrStderr()
	return client.WaitDeleted(cmd.Context(), service, cloudrun.WaitOptions{
		Timeout:  timeout,
		Interval: interval,
		OnUpdate: func(message string) {
			fmt.Fprintf(out, "waiting for %s to be deleted: %s\n", service, message)
		},
		OnRetry: func(err error) {
			fmt.Fprintf(out, "waiting for %s to be deleted: could not read the status, retrying: %v\n", service, err)
		},
	})
}

// waitForRollout はサービスが安定するまで待ち、状態が変わるたびに進捗を stderr へ出す。
// wait (現状のまま待つ) と deploy (適用した世代のロールアウトを待つ) で共有する。
// 成功時は何も出力しない (stdout はデータ専用という規約に従う)。
func waitForRollout(cmd *cobra.Command, client *cloudrun.Client, service string, opts cloudrun.WaitOptions) error {
	out := cmd.ErrOrStderr()
	opts.OnUpdate = func(message string) {
		fmt.Fprintf(out, "waiting for %s: %s\n", service, message)
	}
	// 取得に失敗しても待機は続けるが、黙って再試行すると「止まっている」ように
	// 見えるので知らせる。
	opts.OnRetry = func(err error) {
		fmt.Fprintf(out, "waiting for %s: could not read the status, retrying: %v\n", service, err)
	}
	_, err := client.Wait(cmd.Context(), service, opts)
	return err
}
