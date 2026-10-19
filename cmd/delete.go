package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

// deleteOptions are delete's flags. Each delete command (services, jobs) has its own.
type deleteOptions struct {
	project     string
	region      string
	dryRun      bool
	autoApprove bool
	noWait      bool
	interval    time.Duration
	timeout     time.Duration
}

var deleteCmd = newDeleteCmd(serviceTarget)

// newDeleteCmd builds delete for a target.
func newDeleteCmd(t target) *cobra.Command {
	o := &deleteOptions{}
	goes := t.deleteGoes
	cmd := &cobra.Command{
		Use:   fmt.Sprintf("delete [%s]", t.noun),
		Short: fmt.Sprintf("Delete a %s", t.noun),
		Long: fmt.Sprintf("Delete a Cloud Run %[1]s. This cannot be undone: %[2]s.\n"+
			"What is about to be deleted is printed on stderr and confirmed before anything happens.\n"+
			"Use --auto-approve to skip the prompt (for CI/CD), or --dry-run to validate the request\n"+
			"server-side without deleting anything.\n"+
			"Deletion is asynchronous, so delete waits until the %[1]s is actually gone; pass\n"+
			"--no-wait to return as soon as the request is accepted.\n"+
			"%[1]s may be omitted when set in the config file.", t.noun, goes),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDelete(cmd, args, t, o)
		},
	}
	addTargetFlags(cmd, &o.project, &o.region)
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false,
		"validate the request server-side without deleting anything")
	cmd.Flags().BoolVar(&o.autoApprove, "auto-approve", false,
		"delete without the interactive confirmation prompt (for CI/CD)")
	cmd.Flags().BoolVar(&o.noWait, "no-wait", false,
		fmt.Sprintf("return as soon as the request is accepted, without waiting for the %s to disappear", t.noun))
	cmd.Flags().DurationVar(&o.interval, "interval", defaultRolloutInterval, intervalUsage)
	cmd.Flags().DurationVar(&o.timeout, "timeout", defaultRolloutTimeout,
		timeoutUsage(fmt.Sprintf("the %s to disappear", t.noun)))
	return cmd
}

func runDelete(cmd *cobra.Command, args []string, t target, o *deleteOptions) error {
	name, err := t.resolveName(args)
	if err != nil {
		return err
	}

	if err := validateWaitFlags(o.timeout, o.interval); err != nil {
		return err
	}
	ctx := cmd.Context()
	client, err := newCloudRunClient(cmd, o.project, o.region)
	if err != nil {
		return err
	}

	// Show what is about to be deleted first. A missing resource is detected here, so there is no
	// confirmation prompt for one that does not exist.
	details, err := t.deleteDetails(ctx, client, name)
	if err != nil {
		return err
	}
	printDeleteTarget(cmd, client.Project(), client.Region(), t.noun, name, details)

	// --dry-run deletes nothing, so it does not ask for confirmation (the same policy as deploy).
	if !o.dryRun {
		ok, err := confirmAction(cmd, o.autoApprove, "delete",
			fmt.Sprintf("Delete %s %q? This cannot be undone.", t.noun, name))
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}

	if err := client.Delete(ctx, t.kind, name, o.dryRun); err != nil {
		return err
	}
	// --dry-run has deleted nothing, so there is nothing to wait for.
	if o.dryRun || o.noWait {
		return nil
	}
	return waitForDeletion(cmd, client, t, name, o.timeout, o.interval)
}

// targetDetail is one extra line of what printTarget shows.
type targetDetail struct {
	label, value string
}

// serviceDeleteDetails fetches the service (failing when it does not exist) and returns its URL,
// which goes with it.
func serviceDeleteDetails(ctx context.Context, client *cloudrun.Client, name string) ([]targetDetail, error) {
	status, err := client.Status(ctx, name)
	if err != nil {
		return nil, err
	}
	if status.URL == "" {
		return nil, nil
	}
	return []targetDetail{{"url", status.URL}}, nil
}

// jobDeleteDetails fetches the job (failing when it does not exist) and returns how many
// executions go with it.
func jobDeleteDetails(ctx context.Context, client *cloudrun.Client, name string) ([]targetDetail, error) {
	status, err := client.GetJobStatus(ctx, name)
	if err != nil {
		return nil, err
	}
	return []targetDetail{{"executions", fmt.Sprintf("%d (deleted with the job)", status.ExecutionCount)}}, nil
}

// printDeleteTarget lists what is about to be deleted on stderr.
func printDeleteTarget(cmd *cobra.Command, project, region, noun, name string, details []targetDetail) {
	printTarget(cmd, "About to delete:", project, region, noun, name, details)
}

// printTarget lists what a command is about to act on — the commands that apply a plan, delete,
// prune or cancel, and jobs run — on stderr, under heading. The
// project and region are always printed to prevent the accident of acting on the right name after
// mixing them up.
func printTarget(cmd *cobra.Command, heading, project, region, noun, name string, details []targetDetail) {
	out := cmd.ErrOrStderr()
	rows := append([]targetDetail{{noun, name}, {"project", project}, {"region", region}}, details...)
	width := 0
	for _, r := range rows {
		if len(r.label) > width {
			width = len(r.label)
		}
	}
	fmt.Fprintln(out, heading)
	for _, r := range rows {
		fmt.Fprintf(out, "  %-*s %s\n", width+1, r.label+":", r.value)
	}
}
