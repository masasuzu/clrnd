package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

// Defaults for --timeout / --interval, shared by every command that waits for a rollout or a
// deletion.
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
	return waitForRollout(cmd, service, func(ctx context.Context, opts cloudrun.WaitOptions) (*cloudrun.Status, error) {
		return client.Wait(ctx, service, opts)
	}, cloudrun.WaitOptions{
		Timeout:  waitTimeout,
		Interval: waitInterval,
	})
}

// waitForDeletion waits until the service is actually gone. Deletion in Cloud Run is asynchronous,
// so without this the service can still be fetched right after delete.
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

// waitForRollout runs wait (Client.Wait, or DeployPlan.Wait for whatever kind the plan applies),
// printing progress to stderr whenever the state changes. It is shared by wait (waits on the current
// state) and applyPlan (waits for the rollout of the generation it applied). On success it prints
// nothing (following the convention that stdout is data-only).
func waitForRollout(cmd *cobra.Command, name string,
	wait func(context.Context, cloudrun.WaitOptions) (*cloudrun.Status, error), opts cloudrun.WaitOptions) error {
	out := cmd.ErrOrStderr()
	opts.OnUpdate = func(message string) {
		fmt.Fprintf(out, "waiting for %s: %s\n", name, message)
	}
	// A failed read does not stop the wait, but retrying silently would look like being "stuck",
	// so say so.
	opts.OnRetry = func(err error) {
		fmt.Fprintf(out, "waiting for %s: could not read the status, retrying: %v\n", name, err)
	}
	_, err := wait(cmd.Context(), opts)
	return err
}
