package cmd

import (
	"fmt"
	"time"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

// jobsExecutionsCmd groups the commands for a job's executions, spelled the way gcloud spells them
// (gcloud run jobs executions). Starting one is `jobs run`; these look at and stop the ones that
// exist.
var jobsExecutionsCmd = &cobra.Command{
	Use:   "executions",
	Short: "List and cancel a job's executions",
	Long: "Commands for the executions of a job: list them, and cancel one that is running.\n" +
		"Starting an execution is 'clrnd jobs run'.",
	// The same as jobsCmd: without a Run, a mistyped command would print the help and exit 0.
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

var (
	jobsExecutionsListProject string
	jobsExecutionsListRegion  string
	jobsExecutionsListFormat  string
)

var jobsExecutionsListCmd = &cobra.Command{
	Use:   "list [job]",
	Short: "List the executions of a job",
	Long: "List the executions of a job, newest first: each one's status (Pending, Running,\n" +
		"Cancelling, Succeeded, Failed or Cancelled), its tasks, and when it was created and\n" +
		"completed. This is read-only; nothing is modified. A job that does not exist is an error\n" +
		"rather than an empty list.\n" +
		"job may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(1),
	RunE: runJobsExecutionsList,
}

// cancelOptions are jobs executions cancel's flags.
type cancelOptions struct {
	project     string
	region      string
	autoApprove bool
	noWait      bool
	timeout     time.Duration
	interval    time.Duration
}

var jobsExecutionsCancelCmd = newJobsExecutionsCancelCmd()

func newJobsExecutionsCancelCmd() *cobra.Command {
	o := &cancelOptions{}
	cmd := &cobra.Command{
		Use:   "cancel <execution>",
		Short: "Cancel a running execution",
		Long: "Cancel a running execution of a job: its tasks are stopped and it ends as Cancelled.\n" +
			"The job itself is not changed, and the next run starts as usual.\n" +
			"The execution is fetched first, so one that does not exist, or has already finished,\n" +
			"fails before anything is asked. What is about to be cancelled is printed on stderr and\n" +
			"confirmed; use --auto-approve to skip the prompt (for CI/CD). Cloud Run has no dry run\n" +
			"for a cancel.\n" +
			"The cancel takes effect shortly after it is accepted, so cancel waits until the\n" +
			"execution has stopped, and fails if it succeeded first; pass --no-wait to return as\n" +
			"soon as the request is accepted.\n" +
			"execution is the name 'clrnd jobs run' prints and 'clrnd jobs executions list' shows.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJobsExecutionsCancel(cmd, args[0], o)
		},
	}
	addTargetFlags(cmd, &o.project, &o.region)
	cmd.Flags().BoolVar(&o.autoApprove, "auto-approve", false,
		"cancel without the interactive confirmation prompt (for CI/CD)")
	cmd.Flags().BoolVar(&o.noWait, "no-wait", false,
		"return as soon as the request is accepted, without waiting for the execution to stop")
	cmd.Flags().DurationVar(&o.interval, "interval", defaultRolloutInterval, intervalUsage)
	cmd.Flags().DurationVar(&o.timeout, "timeout", defaultRolloutTimeout, timeoutUsage("the execution to stop"))
	return cmd
}

func init() {
	addTargetFlags(jobsExecutionsListCmd, &jobsExecutionsListProject, &jobsExecutionsListRegion)
	addFormatFlag(jobsExecutionsListCmd, &jobsExecutionsListFormat)
	jobsExecutionsCmd.AddCommand(jobsExecutionsListCmd)
	jobsExecutionsCmd.AddCommand(jobsExecutionsCancelCmd)
}

func runJobsExecutionsList(cmd *cobra.Command, args []string) error {
	job, err := resolveJob(args)
	if err != nil {
		return err
	}
	if err := validateFormat(jobsExecutionsListFormat); err != nil {
		return err
	}
	client, err := newCloudRunClient(cmd, jobsExecutionsListProject, jobsExecutionsListRegion)
	if err != nil {
		return err
	}
	executions, err := client.ListExecutions(cmd.Context(), job)
	if err != nil {
		return err
	}
	return writeFormatted(cmd, jobsExecutionsListFormat, executions, executions.Text())
}

func runJobsExecutionsCancel(cmd *cobra.Command, execution string, o *cancelOptions) error {
	if err := validateWaitFlags(o.timeout, o.interval); err != nil {
		return err
	}
	ctx := cmd.Context()
	client, err := newCloudRunClient(cmd, o.project, o.region)
	if err != nil {
		return err
	}

	// Fetch it first: a missing execution fails here, and one that has finished is refused with
	// its state rather than with the API's bare 400, both before anything is asked.
	e, err := client.GetExecution(ctx, execution)
	if err != nil {
		return err
	}
	if e.Finished() {
		return fmt.Errorf("execution %q has already finished (%s); there is nothing to cancel", execution, e.Status)
	}
	printTarget(cmd, "About to cancel:", client.Project(), client.Region(), "execution", execution,
		[]targetDetail{{"job", e.Job}, {"status", fmt.Sprintf("%s (%s)", e.Status, e.TaskSummary())}})

	ok, err := confirmAction(cmd, o.autoApprove, "cancel",
		fmt.Sprintf("Cancel execution %q? Its running tasks are stopped.", execution))
	if err != nil || !ok {
		return err
	}
	if err := client.CancelExecution(ctx, execution); err != nil {
		return err
	}
	if o.noWait {
		return nil
	}

	out := cmd.ErrOrStderr()
	_, err = client.WaitExecutionStopped(ctx, execution, cloudrun.WaitOptions{
		Timeout:  o.timeout,
		Interval: o.interval,
		OnUpdate: func(message string) {
			fmt.Fprintf(out, "waiting for %s to stop: %s\n", execution, message)
		},
		OnRetry: func(err error) {
			fmt.Fprintf(out, "waiting for %s to stop: could not read the status, retrying: %v\n", execution, err)
		},
	})
	return err
}
