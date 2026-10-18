package cmd

import "github.com/spf13/cobra"

var (
	jobsStatusProject string
	jobsStatusRegion  string
	jobsStatusFormat  string
)

// jobsStatusCmd is status for a job. It is its own command rather than status built for jobs: a
// job's state (its executions) has nothing in common with a service's (revisions, traffic, URL)
// beyond the Ready condition.
var jobsStatusCmd = &cobra.Command{
	Use:   "status [job]",
	Short: "Show the current status of a job",
	Long: "Fetch the job from Cloud Run and print its current state: the Ready condition, the\n" +
		"observed generation, how many executions it has had and how the last one went, and every\n" +
		"status condition. This is read-only; nothing is modified.\n" +
		"job may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(1),
	RunE: runJobsStatus,
}

func init() {
	addTargetFlags(jobsStatusCmd, &jobsStatusProject, &jobsStatusRegion)
	addFormatFlag(jobsStatusCmd, &jobsStatusFormat)
}

func runJobsStatus(cmd *cobra.Command, args []string) error {
	job, err := resolveJob(args)
	if err != nil {
		return err
	}
	if err := validateFormat(jobsStatusFormat); err != nil {
		return err
	}
	client, err := newCloudRunClient(cmd, jobsStatusProject, jobsStatusRegion)
	if err != nil {
		return err
	}
	status, err := client.GetJobStatus(cmd.Context(), job)
	if err != nil {
		return err
	}
	return writeFormatted(cmd, jobsStatusFormat, status, status.Text())
}
