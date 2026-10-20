package cmd

import "github.com/spf13/cobra"

// workerPoolsCmd groups the commands for Cloud Run worker pools; gcloud spells the group the same
// way (gcloud run worker-pools).
//
// A worker pool is deployed, inspected and removed the same way a service is, so deploy, diff,
// verify, wait, delete and init are the services' commands built for worker pools (newDeployCmd and
// friends). It has revisions like a service but no URL and no traffic: what it splits between
// revisions is its instances, which have no command here yet.
var workerPoolsCmd = &cobra.Command{
	Use:   "worker-pools",
	Short: "Deploy Cloud Run worker pools",
	Long: "Commands for Cloud Run worker pools (kind: WorkerPool), which run background work with no\n" +
		"URL and a fixed instance count. They take the worker pool's name and a manifest the same\n" +
		"way the top-level commands take a service's; in the config file, name the worker pool with\n" +
		"workerPool: instead of service:.",
	// Without a Run of its own, cobra never validates a group's arguments; see jobsCmd.
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

var (
	workerPoolsStatusProject string
	workerPoolsStatusRegion  string
	workerPoolsStatusFormat  string
)

// workerPoolsStatusCmd is status for a worker pool. It is its own command rather than status built
// for worker pools: what a worker pool reports (its instance count and how the instances are split
// between revisions) is not what a service reports (its URL and traffic).
var workerPoolsStatusCmd = &cobra.Command{
	Use:   "status [worker-pool]",
	Short: "Show the current status of a worker pool",
	Long: "Fetch the worker pool from Cloud Run and print its current state: the Ready condition, the\n" +
		"instance count, the latest ready and created revisions, the observed generation, how the\n" +
		"instances are split between revisions, and every status condition. This is read-only;\n" +
		"nothing is modified.\n" +
		"worker-pool may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(1),
	RunE: runWorkerPoolsStatus,
}

func init() {
	addTargetFlags(workerPoolsStatusCmd, &workerPoolsStatusProject, &workerPoolsStatusRegion)
	addFormatFlag(workerPoolsStatusCmd, &workerPoolsStatusFormat)

	workerPoolsCmd.AddCommand(newVerifyCmd(workerPoolTarget))
	workerPoolsCmd.AddCommand(newDiffCmd(workerPoolTarget))
	workerPoolsCmd.AddCommand(newDeployCmd(workerPoolTarget))
	workerPoolsCmd.AddCommand(workerPoolsStatusCmd)
	workerPoolsCmd.AddCommand(newWaitCmd(workerPoolTarget))
	workerPoolsCmd.AddCommand(newDeleteCmd(workerPoolTarget))
	workerPoolsCmd.AddCommand(newInitCmd(workerPoolTarget))
}

func runWorkerPoolsStatus(cmd *cobra.Command, args []string) error {
	pool, err := resolveWorkerPool(args)
	if err != nil {
		return err
	}
	if err := validateFormat(workerPoolsStatusFormat); err != nil {
		return err
	}
	client, err := newCloudRunClient(cmd, workerPoolsStatusProject, workerPoolsStatusRegion)
	if err != nil {
		return err
	}
	status, err := client.GetWorkerPoolStatus(cmd.Context(), pool)
	if err != nil {
		return err
	}
	return writeFormatted(cmd, workerPoolsStatusFormat, status, status.Text())
}
