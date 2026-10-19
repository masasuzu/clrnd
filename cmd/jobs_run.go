package cmd

import (
	"fmt"
	"time"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

var (
	jobsRunProject     string
	jobsRunRegion      string
	jobsRunContainer   string
	jobsRunArgs        []string
	jobsRunEnv         []string
	jobsRunTaskCount   int64
	jobsRunTaskTimeout time.Duration
	jobsRunNoWait      bool
	jobsRunTimeout     time.Duration
	jobsRunInterval    time.Duration
)

var jobsRunCmd = &cobra.Command{
	Use: "run [job]",
	// gcloud calls this "gcloud run jobs execute"; ecspresso and the API call it run.
	Aliases: []string{"execute"},
	Short:   "Run a job and wait for the execution to finish",
	Long: "Start an execution of the job and wait until it finishes. The command fails when the\n" +
		"execution fails or is cancelled, so a CI step that runs a job (a migration, say) fails\n" +
		"with it. The execution's name is printed on stdout as soon as it is created; progress goes\n" +
		"to stderr.\n" +
		"There is no time limit on the wait by default: the job's own task timeout and retries\n" +
		"bound the execution, and giving up here would not stop it. --timeout sets one anyway.\n" +
		"Stopping the wait — Ctrl-C, or --timeout — never cancels the execution; the error says\n" +
		"how to. --no-wait returns as soon as the execution is created.\n" +
		"--args, --env, --task-count and --task-timeout override the job for this execution only;\n" +
		"the job's definition is not changed. Running with them needs run.jobs.runWithOverrides.\n" +
		"There is no confirmation prompt, as with gcloud and ecspresso: running a job does not\n" +
		"change any definition.\n" +
		"job may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(1),
	RunE: runJobsRun,
}

func init() {
	addTargetFlags(jobsRunCmd, &jobsRunProject, &jobsRunRegion)
	f := jobsRunCmd.Flags()
	f.StringArrayVar(&jobsRunArgs, "args", nil,
		"an argument for the container, replacing the job's arguments (repeatable, one argument each)")
	f.StringArrayVar(&jobsRunEnv, "env", nil,
		"NAME=VALUE environment variable to set for this execution (repeatable)")
	f.StringVar(&jobsRunContainer, "container", "",
		"the container --args and --env apply to (needed when the job has more than one)")
	f.Int64Var(&jobsRunTaskCount, "task-count", 0, "number of tasks to run (default: the job's own)")
	f.DurationVar(&jobsRunTaskTimeout, "task-timeout", 0,
		"how long each task may run, in whole seconds (default: the job's own)")
	f.BoolVar(&jobsRunNoWait, "no-wait", false, "return as soon as the execution is created")
	f.DurationVar(&jobsRunTimeout, "timeout", 0, "give up waiting after this long (default: no limit)")
	f.DurationVar(&jobsRunInterval, "interval", defaultRolloutInterval,
		"initial polling interval (it backs off up to 15s)")
}

func runJobsRun(cmd *cobra.Command, args []string) error {
	job, err := resolveJob(args)
	if err != nil {
		return err
	}
	// Check the overrides before creating the client (= ADC discovery), like every other local
	// check, so that a flag mistake is not hidden behind a credentials error.
	opts := cloudrun.RunOptions{
		Container:   jobsRunContainer,
		Args:        jobsRunArgs,
		TaskCount:   jobsRunTaskCount,
		TaskTimeout: jobsRunTaskTimeout,
	}
	for _, spec := range jobsRunEnv {
		e, err := cloudrun.ParseEnvVar(spec)
		if err != nil {
			return err
		}
		opts.Env = append(opts.Env, e)
	}
	if cmd.Flags().Changed("task-count") && jobsRunTaskCount < 1 {
		// 0 means "the job's own" internally, so an explicit 0 would otherwise be ignored.
		return fmt.Errorf("--task-count must be at least 1, got %d", jobsRunTaskCount)
	}
	if cmd.Flags().Changed("task-timeout") && jobsRunTaskTimeout <= 0 {
		return fmt.Errorf("--task-timeout must be positive, got %s", jobsRunTaskTimeout)
	}
	if err := opts.Validate(); err != nil {
		return err
	}
	if jobsRunTimeout < 0 {
		return fmt.Errorf("--timeout must not be negative, got %s", jobsRunTimeout)
	}

	client, err := newCloudRunClient(cmd, jobsRunProject, jobsRunRegion)
	if err != nil {
		return err
	}
	ctx := cmd.Context()

	// Running a job is how a migration runs, so say where, as the commands that change a
	// definition do. stderr, so stdout keeps only the execution's name.
	printTarget(cmd, "Target:", client.Project(), client.Region(), "job", job, nil)
	execution, err := client.RunJob(ctx, job, opts)
	if err != nil {
		return err
	}
	// The name is data (a script can capture it), so it goes to stdout, and it goes out before the
	// wait: whatever happens to the wait, the user knows which execution to look at.
	fmt.Fprintln(cmd.OutOrStdout(), execution.Name)
	if jobsRunNoWait {
		return nil
	}

	out := cmd.ErrOrStderr()
	_, err = client.WaitExecution(ctx, execution.Name, cloudrun.WaitOptions{
		Timeout:  jobsRunTimeout,
		Interval: jobsRunInterval,
		OnUpdate: func(message string) {
			fmt.Fprintf(out, "waiting for %s: %s\n", execution.Name, message)
		},
		OnRetry: func(err error) {
			fmt.Fprintf(out, "waiting for %s: could not read the status, retrying: %v\n", execution.Name, err)
		},
	})
	return err
}
