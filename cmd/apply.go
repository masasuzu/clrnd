package cmd

import (
	"fmt"
	"time"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

// applyOptions holds the options of the apply flow, which every command that changes a service
// through a plan shares (see applyPlan).
type applyOptions struct {
	DryRun      bool
	AutoApprove bool
	NoWait      bool
	Timeout     time.Duration
	Interval    time.Duration
	// Prompt is the text of the confirmation prompt.
	Prompt string
}

// addApplyFlags registers the flags shared by the apply flow.
func addApplyFlags(cmd *cobra.Command, o *applyOptions) {
	cmd.Flags().BoolVar(&o.DryRun, "dry-run", false,
		"validate the request server-side without applying changes")
	cmd.Flags().BoolVar(&o.AutoApprove, "auto-approve", false,
		"apply without the interactive confirmation prompt (for CI/CD)")
	cmd.Flags().BoolVar(&o.NoWait, "no-wait", false,
		"return as soon as the request is accepted, without waiting for the rollout")
	cmd.Flags().DurationVar(&o.Interval, "interval", defaultRolloutInterval,
		"how long to wait between rollout polls")
	cmd.Flags().DurationVar(&o.Timeout, "timeout", defaultRolloutTimeout,
		"how long to wait for the rollout to finish")
}

// addServerDefaultsFlag registers --no-server-defaults. It is shared by diff and deploy.
// Server defaults are resolved by default, so the flag is the one that turns it off (the same
// shape as --no-wait).
func addServerDefaultsFlag(cmd *cobra.Command, skip *bool) {
	cmd.Flags().BoolVar(skip, "no-server-defaults", false,
		"compare against the manifest as written, without asking Cloud Run to fill in the fields "+
			"it defaults (avoids the dry-run write, so read-only credentials are enough)")
}

// confirmAction asks for confirmation before an irreversible operation. It skips the prompt when
// autoApprove is set, and refuses where confirmation is impossible (a non-interactive stdin).
// When ok is false the caller aborts (the abort message has already been printed). action is the
// verb embedded in the error message.
func confirmAction(cmd *cobra.Command, autoApprove bool, action, prompt string) (bool, error) {
	if autoApprove {
		return true, nil
	}
	if !isInteractive(cmd) {
		return false, fmt.Errorf(
			"refusing to %s without confirmation: re-run with --auto-approve (no interactive terminal)", action)
	}
	ok, err := confirm(cmd.Context(), cmd, prompt)
	if err != nil {
		return false, err
	}
	if !ok {
		fmt.Fprintln(cmd.ErrOrStderr(), "Aborted.")
	}
	return ok, nil
}

// applyPlan prints the diff to stdout, confirms when needed, applies, and waits for the rollout.
// Status and prompts go to stderr; stdout is for data (the diff) only.
func applyPlan(cmd *cobra.Command, client *cloudrun.Client, plan *cloudrun.DeployPlan, o applyOptions) error {
	ctx := cmd.Context()

	// With no diff there is nothing to apply. --dry-run still goes ahead, for server-side
	// validation.
	if plan.Diff == "" {
		fmt.Fprintln(cmd.ErrOrStderr(), "No changes.")
		if o.DryRun {
			_, err := plan.Apply(ctx, o.DryRun)
			return err
		}
		if o.NoWait {
			return nil
		}
		// There is nothing to apply, but still check that the service is healthy.
		// Re-running with the same manifest after a failed deploy produces an empty diff, so
		// letting this case through would report a broken service as a success (defeating the
		// point of adding the wait).
		// No Generation is given: only "is it Ready now", whatever the generation.
		return waitForRollout(cmd, client, plan.Service, cloudrun.WaitOptions{
			Timeout:  o.Timeout,
			Interval: o.Interval,
		})
	}
	fmt.Fprint(cmd.OutOrStdout(), plan.Diff)

	// Confirm unless this is a dry run. --auto-approve skips it.
	if !o.DryRun {
		ok, err := confirmAction(cmd, o.AutoApprove, "apply", o.Prompt)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}

	applied, err := plan.Apply(ctx, o.DryRun)
	if err != nil {
		return err
	}
	// --dry-run only validates server-side and changes nothing, so there is nothing to wait for.
	if o.DryRun || o.NoWait {
		return nil
	}
	return waitForRollout(cmd, client, plan.Service, cloudrun.WaitOptions{
		Timeout:    o.Timeout,
		Interval:   o.Interval,
		Generation: cloudrun.AppliedGeneration(applied),
	})
}
