package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

var (
	deleteProject     string
	deleteRegion      string
	deleteDryRun      bool
	deleteAutoApprove bool
	deleteNoWait      bool
	deleteInterval    time.Duration
	deleteTimeout     time.Duration
)

var deleteCmd = &cobra.Command{
	Use:   "delete [service]",
	Short: "Delete a service",
	Long: "Delete a Cloud Run service. This cannot be undone: the service, all of its revisions,\n" +
		"and its URL go away.\n" +
		"What is about to be deleted is printed on stderr and confirmed before anything happens.\n" +
		"Use --auto-approve to skip the prompt (for CI/CD), or --dry-run to validate the request\n" +
		"server-side without deleting anything.\n" +
		"Deletion is asynchronous, so delete waits until the service is actually gone; pass\n" +
		"--no-wait to return as soon as the request is accepted.\n" +
		"service may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(1),
	RunE: runDelete,
}

func init() {
	addTargetFlags(deleteCmd, &deleteProject, &deleteRegion)
	deleteCmd.Flags().BoolVar(&deleteDryRun, "dry-run", false,
		"validate the request server-side without deleting anything")
	deleteCmd.Flags().BoolVar(&deleteAutoApprove, "auto-approve", false,
		"delete without the interactive confirmation prompt (for CI/CD)")
	deleteCmd.Flags().BoolVar(&deleteNoWait, "no-wait", false,
		"return as soon as the request is accepted, without waiting for the service to disappear")
	deleteCmd.Flags().DurationVar(&deleteInterval, "interval", defaultRolloutInterval,
		"how long to wait between polls while the deletion completes")
	deleteCmd.Flags().DurationVar(&deleteTimeout, "timeout", defaultRolloutTimeout,
		"how long to wait for the service to disappear")
}

func runDelete(cmd *cobra.Command, args []string) error {
	service, err := resolveService(args)
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	client, err := newCloudRunClient(cmd, deleteProject, deleteRegion)
	if err != nil {
		return err
	}

	// Show what is about to be deleted first. A missing service is detected here, so there is no
	// confirmation prompt for a service that does not exist.
	status, err := client.Status(ctx, service)
	if err != nil {
		return err
	}
	printDeleteTarget(cmd, client.Project(), client.Region(), service, status.URL)

	// --dry-run deletes nothing, so it does not ask for confirmation (the same policy as deploy).
	if !deleteDryRun {
		ok, err := confirmAction(cmd, deleteAutoApprove, "delete",
			fmt.Sprintf("Delete service %q? This cannot be undone.", service))
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}

	if err := client.DeleteService(ctx, service, deleteDryRun); err != nil {
		return err
	}
	// --dry-run has deleted nothing, so there is nothing to wait for.
	if deleteDryRun || deleteNoWait {
		return nil
	}
	return waitForDeletion(cmd, client, service, deleteTimeout, deleteInterval)
}

// printDeleteTarget lists what is about to be deleted on stderr. The project and region are always
// printed to prevent the accident of deleting something after mixing them up.
func printDeleteTarget(cmd *cobra.Command, project, region, service, url string) {
	out := cmd.ErrOrStderr()
	fmt.Fprintln(out, "About to delete:")
	fmt.Fprintf(out, "  service: %s\n", service)
	fmt.Fprintf(out, "  project: %s\n", project)
	fmt.Fprintf(out, "  region:  %s\n", region)
	if url != "" {
		fmt.Fprintf(out, "  url:     %s\n", url)
	}
}
