package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/masasuzu/clrnd/internal/config"
)

// target is the kind of resource a command works on: services (the top-level commands), jobs
// (under 'clrnd jobs') or worker pools (under 'clrnd worker-pools'). The commands every kind has —
// deploy, diff, verify, wait, delete and init — are built once per target from the same code, so
// what they do for one kind cannot drift from what they do for another.
//
// Everything those commands do differently for one kind is a field here, describing what the kind
// has or how it is named, rather than a check of which kind it is: a target says whether it has
// traffic, what deploy waits for, and so on.
type target struct {
	kind cloudrun.Kind
	// noun is what help and messages call one ("service", "job", "worker pool").
	noun string
	// configKey is the config file key that names one ("service", "job", "workerPool"), and configName the field
	// behind it, for reading the name and for init to write it.
	configKey  string
	configName func(*config.Config) *string
	// commands is how to point someone at this kind's commands, for an error that finds the
	// config naming this kind while another kind's command runs.
	commands string

	// split describes how the kind's revisions share what it runs, for revisions and rollback. A
	// target with one has revisions: a manifest may pin a revision name, which verify and deploy
	// warn about. nil for a target without revisions.
	split *revisionSplit
	// waitFor is what deploy waits for after applying, as its help puts it.
	waitFor string
	// deployLong is deploy's help text.
	deployLong string
	// manifestFormat is how init's help calls the manifest it writes.
	manifestFormat string
	// initAliases are init's other names: load, init's old name, only ever covered services.
	initAliases []string
	// deleteGoes is what delete's help says goes with the resource.
	deleteGoes string
	// deleteDetails fetches the resource (failing when it does not exist) and returns what is worth
	// showing about it before delete removes it.
	deleteDetails func(ctx context.Context, client *cloudrun.Client, name string) ([]targetDetail, error)
	// resultName is the field of verify's result that names what was verified.
	resultName func(*verifyResult) *string
}

var (
	serviceTarget = target{
		kind:           cloudrun.KindService,
		noun:           "service",
		configKey:      "service",
		configName:     func(c *config.Config) *string { return &c.Service },
		commands:       "the top-level commands such as 'clrnd deploy'",
		split:          serviceSplit,
		waitFor:        "the rollout",
		deployLong:     serviceDeployLong,
		manifestFormat: "Knative-style YAML",
		initAliases:    []string{"load"},
		deleteGoes:     "the service, all of its revisions,\nand its URL go away",
		deleteDetails:  serviceDeleteDetails,
		resultName:     func(r *verifyResult) *string { return &r.Service },
	}
	jobTarget = target{
		kind:           cloudrun.KindJob,
		noun:           "job",
		configKey:      "job",
		configName:     func(c *config.Config) *string { return &c.Job },
		commands:       "the 'clrnd jobs' commands",
		waitFor:        "the job to be ready",
		deployLong:     jobDeployLong,
		manifestFormat: "YAML",
		deleteGoes:     "the job and all of its executions go\naway",
		deleteDetails:  jobDeleteDetails,
		resultName:     func(r *verifyResult) *string { return &r.Job },
	}
	workerPoolTarget = target{
		kind:           cloudrun.KindWorkerPool,
		noun:           "worker pool",
		configKey:      "workerPool",
		configName:     func(c *config.Config) *string { return &c.WorkerPool },
		commands:       "the 'clrnd worker-pools' commands",
		split:          workerPoolSplit,
		waitFor:        "the rollout",
		deployLong:     workerPoolDeployLong,
		manifestFormat: "YAML",
		deleteGoes:     "the worker pool, all of its\nrevisions and its running instances go away",
		deleteDetails:  workerPoolDeleteDetails,
		resultName:     func(r *verifyResult) *string { return &r.WorkerPool },
	}
	// targets are every target, for the check that the config file does not name another kind.
	targets = []target{serviceTarget, jobTarget, workerPoolTarget}
)

// revisionSplit is how help and messages put what a kind splits between its revisions: a
// service's traffic, a worker pool's instances.
type revisionSplit struct {
	listed    string // what revisions lists for each revision
	protected string // the revisions a prune never deletes
	serving   string // a revision with a share
	serve     string // what a revision has to be able to do
	// rollback's help and prompt (rollbackPrompt takes the revision's name).
	rollbackShort, rollbackLong, rollbackKeeps, rollbackPrompt string
	// table formats a list of revisions as text.
	table func(cloudrun.Revisions) string

	// The command that changes the split (newSplitCmd): its name and help.
	command, commandShort, commandLong string
	what                               string // what is split ("traffic")
	verb                               string // a revision having a share, before "the most" ("serving")
	sendTo                             string // giving a revision a share ("send traffic to")
	toLatestUsage                      string // --to-latest's help
	splitPrompt                        string // the confirmation; takes the percentage and the revision
	// deploy's flag that keeps the current split (keepFlag, without the dashes), its help, and the
	// warning that it replaces the split the manifest writes.
	keepFlag, keepUsage, keepWarning string
}

var (
	serviceSplit = &revisionSplit{
		listed:         "the share of traffic each one currently receives and its traffic tags",
		protected:      "serving traffic, carrying a tag, or named in spec.traffic",
		serving:        "receiving traffic",
		serve:          "serve traffic",
		rollbackShort:  "Send traffic back to an earlier revision",
		rollbackLong:   "Send all traffic back to an earlier revision.",
		rollbackKeeps:  ", and traffic tags are kept (pinned at 0%)",
		rollbackPrompt: "Send all traffic to %s?",
		table:          cloudrun.Revisions.Text,
		command:        "traffic",
		commandShort:   "Change how traffic is split between revisions",
		commandLong: "Send a share of the traffic to a revision. Only spec.traffic changes: no new revision\n" +
			"is created, and traffic tags are kept (pinned at 0%).\n" +
			"--percent below 100 leaves the rest on the revision that is currently serving the most,\n" +
			"which is the canary shape (new 10% / stable 90%).\n" +
			"--to-latest follows whatever the newest revision is, so it is also how you undo a\n" +
			"rollback: the traffic stops being pinned to the older revision.\n" +
			"The diff is shown and confirmed the same way deploy does, and the rollout is waited\n" +
			"for unless --no-wait is given.\n" +
			"service may be omitted when set in the config file.",
		what:          "traffic",
		verb:          "serving",
		sendTo:        "send traffic to",
		toLatestUsage: "send traffic to the latest revision, and keep following it",
		splitPrompt:   "Send %d%% of the traffic to %s?",
		keepFlag:      "no-traffic",
		keepUsage:     "create the revision without sending traffic to it (keep the current split)",
		keepWarning: "--no-traffic replaces the spec.traffic written in the manifest with the " +
			"split the service is serving now",
	}
	workerPoolSplit = &revisionSplit{
		listed:         "the share of the instances each one runs",
		protected:      "running instances or named in spec.instanceSplits",
		serving:        "running instances",
		serve:          "run instances",
		rollbackShort:  "Run a worker pool's instances on an earlier revision again",
		rollbackLong:   "Run every instance on an earlier revision.",
		rollbackPrompt: "Run every instance on %s?",
		table:          cloudrun.Revisions.InstancesText,
		command:        "instance-split",
		commandShort:   "Change how a worker pool's instances are split between revisions",
		commandLong: "Run a share of the instances on a revision. Only spec.instanceSplits changes: no new\n" +
			"revision is created, and the instance count stays as it is.\n" +
			"--percent below 100 leaves the rest on the revision that is currently running the most,\n" +
			"which is the canary shape (new 10% / stable 90%). Every revision with a share needs an\n" +
			"instance of its own, so a split between two revisions needs at least 2 instances.\n" +
			"--to-latest follows whatever the newest revision is, so it is also how you undo a\n" +
			"rollback or a deploy --no-promote: the instances stop being pinned to the older revision.\n" +
			"The diff is shown and confirmed the same way deploy does, and the rollout is waited\n" +
			"for unless --no-wait is given.\n" +
			"worker-pool may be omitted when set in the config file.",
		what:          "instances",
		verb:          "running",
		sendTo:        "run the instances on",
		toLatestUsage: "run the instances on the latest revision, and keep following it",
		splitPrompt:   "Run %d%% of the instances on %s?",
		keepFlag:      "no-promote",
		keepUsage:     "create the revision without running instances on it (keep the current split)",
		keepWarning: "--no-promote replaces the spec.instanceSplits written in the manifest with the " +
			"split the worker pool runs now",
	}
)

// arg is how usage lines and help name the positional argument: the noun, hyphenated, as gcloud
// writes a worker pool's (WORKER_POOL in its own help, worker-pool here).
func (t target) arg() string { return strings.ReplaceAll(t.noun, " ", "-") }

// resolveName resolves the name in the order positional args[0] > the config file's key for this
// target.
//
// When the config names another kind instead, it is an error rather than an empty result: falling
// back to that name would point this command (delete, say) at a resource of this kind that merely
// shares the other's name.
func (t target) resolveName(args []string) (string, error) {
	if len(args) >= 1 && args[0] != "" {
		return args[0], nil
	}
	if name := *t.configName(cfg); name != "" {
		return name, nil
	}
	for _, other := range targets {
		if other.kind == t.kind {
			continue
		}
		if name := *other.configName(cfg); name != "" {
			return "", fmt.Errorf("%s is required: the config file names a %s (%s: %s); use %s for it",
				t.noun, other.noun, other.configKey, name, other.commands)
		}
	}
	return "", fmt.Errorf("%s is required: pass it as an argument or set %s in the config file", t.noun, t.configKey)
}

// resolveService resolves the service for a command only services have.
func resolveService(args []string) (string, error) { return serviceTarget.resolveName(args) }

// resolveJob resolves the job for a command only jobs have.
func resolveJob(args []string) (string, error) { return jobTarget.resolveName(args) }

// resolveWorkerPool resolves the worker pool for a command only worker pools have.
func resolveWorkerPool(args []string) (string, error) { return workerPoolTarget.resolveName(args) }
