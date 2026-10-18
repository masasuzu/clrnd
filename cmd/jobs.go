package cmd

import "github.com/spf13/cobra"

// jobsCmd groups the commands for Cloud Run jobs. The top-level commands stay the ones for
// services, so existing invocations keep working; gcloud spells the group the same way
// (gcloud run jobs).
//
// A job is deployed the same way a service is, so deploy, diff and verify are the services'
// commands built for jobs (newDeployCmd and friends). What a service has and a job does not —
// revisions, traffic — has no counterpart here, and what a job has and a service does not — running it —
// is its own command (jobsRunCmd).
var jobsCmd = &cobra.Command{
	Use:   "jobs",
	Short: "Deploy and run Cloud Run jobs",
	Long: "Commands for Cloud Run jobs (kind: Job). They take the job's name and a manifest the\n" +
		"same way the top-level commands take a service's; in the config file, name the job with\n" +
		"job: instead of service:.",
}

func init() {
	jobsCmd.AddCommand(newVerifyCmd(jobTarget))
	jobsCmd.AddCommand(newDiffCmd(jobTarget))
	jobsCmd.AddCommand(newDeployCmd(jobTarget))
	jobsCmd.AddCommand(jobsRunCmd)
}
