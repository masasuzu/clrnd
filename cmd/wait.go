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

// waitOptions are wait's flags. Each wait command (services, jobs) has its own.
type waitOptions struct {
	project  string
	region   string
	timeout  time.Duration
	interval time.Duration
}

var waitCmd = newWaitCmd(serviceTarget)

// newWaitCmd builds wait for a target: it waits for what deploy waits for after applying, for
// when deploy ran with --no-wait or something else changed the resource.
func newWaitCmd(t target) *cobra.Command {
	o := &waitOptions{}
	cmd := &cobra.Command{
		Use:   fmt.Sprintf("wait [%s]", t.arg()),
		Short: fmt.Sprintf("Wait until a %s is ready", t.noun),
		Long: fmt.Sprintf("Poll the %[1]s until its Ready condition becomes True. If it becomes False the\n"+
			"command fails immediately instead of waiting for the timeout. Progress is reported\n"+
			"on stderr; nothing is written to stdout. Ctrl-C stops the wait.\n"+
			"%[2]s may be omitted when set in the config file.", t.noun, t.arg()),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWait(cmd, args, t, o)
		},
	}
	addTargetFlags(cmd, &o.project, &o.region)
	cmd.Flags().DurationVar(&o.timeout, "timeout", defaultRolloutTimeout, timeoutUsage(t.waitFor))
	cmd.Flags().DurationVar(&o.interval, "interval", defaultRolloutInterval, intervalUsage)
	return cmd
}

func runWait(cmd *cobra.Command, args []string, t target, o *waitOptions) error {
	name, err := t.resolveName(args)
	if err != nil {
		return err
	}
	if err := validateWaitFlags(o.timeout, o.interval); err != nil {
		return err
	}
	client, err := newCloudRunClient(cmd, o.project, o.region)
	if err != nil {
		return err
	}
	return waitForRollout(cmd, name, func(ctx context.Context, opts cloudrun.WaitOptions) (*cloudrun.Status, error) {
		return client.Wait(ctx, t.kind, name, opts)
	}, cloudrun.WaitOptions{
		Timeout:  o.timeout,
		Interval: o.interval,
	})
}

// waitForDeletion waits until the service or job is actually gone. Deletion in Cloud Run is
// asynchronous, so without this it can still be fetched right after delete.
func waitForDeletion(cmd *cobra.Command, client *cloudrun.Client, t target, name string,
	timeout, interval time.Duration) error {
	out := cmd.ErrOrStderr()
	return client.WaitDeleted(cmd.Context(), t.kind, name, cloudrun.WaitOptions{
		Timeout:  timeout,
		Interval: interval,
		OnUpdate: func(message string) {
			fmt.Fprintf(out, "waiting for %s to be deleted: %s\n", name, message)
		},
		OnRetry: func(err error) {
			fmt.Fprintf(out, "waiting for %s to be deleted: could not read the status, retrying: %v\n", name, err)
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
