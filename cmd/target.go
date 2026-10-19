package cmd

import (
	"context"
	"fmt"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/masasuzu/clrnd/internal/config"
)

// target is the kind of resource a command works on: services (the top-level commands) or jobs
// (under 'clrnd jobs'). The commands both kinds have — deploy, diff, verify, delete and init — are
// built once per target from the same code, so what they do for a job cannot drift from what they
// do for a service.
//
// Everything those commands do differently for one kind is a field here, describing what the kind
// has or how it is named, rather than a check of which kind it is. Another kind (a worker pool,
// say) is then one more target: it says whether it has traffic, what deploy waits for, and so on,
// instead of turning every "is it a service" into a three-way switch.
type target struct {
	kind cloudrun.Kind
	// noun is what help and messages call one ("service", "job").
	noun string
	// configKey is the config file key that names one ("service", "job"), and configName the field
	// behind it, for reading the name and for init to write it.
	configKey  string
	configName func(*config.Config) *string
	// commands is how to point someone at this kind's commands, for an error that finds the
	// config naming this kind while another kind's command runs.
	commands string

	// hasRevisions: a manifest may pin a revision name, which verify and deploy warn about.
	hasRevisions bool
	// hasTraffic: deploy offers --no-traffic.
	hasTraffic bool
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
		hasRevisions:   true,
		hasTraffic:     true,
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
	// targets are every target, for the check that the config file does not name another kind.
	targets = []target{serviceTarget, jobTarget}
)

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
