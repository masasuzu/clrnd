package cmd

import "github.com/spf13/cobra"

// jobsCmd groups the commands for Cloud Run jobs. The top-level commands stay the ones for
// services, so existing invocations keep working; gcloud spells the group the same way
// (gcloud run jobs).
//
// A job is deployed, inspected and removed the same way a service is, so deploy, diff, verify,
// delete and init are the services' commands built for jobs (newDeployCmd and friends). What a
// service has and a job does not — revisions, traffic — has no counterpart here, and what a job has
// and a service does not — running it, and its executions — are commands of their own (jobsRunCmd,
// jobsExecutionsCmd).
var jobsCmd = &cobra.Command{
	Use:   "jobs",
	Short: "Deploy and run Cloud Run jobs",
	Long: "Commands for Cloud Run jobs (kind: Job). They take the job's name and a manifest the\n" +
		"same way the top-level commands take a service's; in the config file, name the job with\n" +
		"job: instead of service:.",
	// Without a Run of its own, cobra never validates a group's arguments: `clrnd jobs lod my-job`
	// would print the help and exit 0, so a typo in a CI script would pass silently. NoArgs turns
	// that into "unknown command", and a bare `clrnd jobs` still shows the help.
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

func init() {
	jobsCmd.AddCommand(newVerifyCmd(jobTarget))
	jobsCmd.AddCommand(newDiffCmd(jobTarget))
	jobsCmd.AddCommand(newDeployCmd(jobTarget))
	jobsCmd.AddCommand(jobsRunCmd)
	jobsCmd.AddCommand(jobsStatusCmd)
	jobsCmd.AddCommand(newDeleteCmd(jobTarget))
	jobsCmd.AddCommand(newInitCmd(jobTarget))
	jobsCmd.AddCommand(jobsExecutionsCmd)
}
