package cmd

import (
	"fmt"

	"github.com/masasuzu/clrnd/internal/cloudrun"
)

// target is the kind of resource a command works on: services (the top-level commands) or jobs
// (under 'clrnd jobs'). deploy, diff and verify are built once per target from the same code, so
// what they do for a job cannot drift from what they do for a service.
type target struct {
	kind cloudrun.Kind
	// noun is what help and messages call one ("service", "job").
	noun string
	// resolveName resolves the name from the positional arguments, then from the config file.
	resolveName func(args []string) (string, error)
}

var (
	serviceTarget = target{kind: cloudrun.KindService, noun: "service", resolveName: resolveService}
	jobTarget     = target{kind: cloudrun.KindJob, noun: "job", resolveName: resolveJob}
)

// isService reports whether the target is services, which have the revision and traffic features
// a job lacks.
func (t target) isService() bool {
	return t.kind == cloudrun.KindService
}

// resolveService resolves the service in the order positional args[0] > config service.
func resolveService(args []string) (string, error) {
	if len(args) >= 1 && args[0] != "" {
		return args[0], nil
	}
	if cfg.Service != "" {
		return cfg.Service, nil
	}
	// The config names a job. Falling back to it would point a service command (delete, say) at
	// a service that merely shares the job's name.
	if cfg.Job != "" {
		return "", fmt.Errorf("service is required: the config file names a job (job: %s); "+
			"use the 'clrnd jobs' commands for it", cfg.Job)
	}
	return "", fmt.Errorf("service is required: pass it as an argument or set service in the config file")
}

// resolveJob resolves the job in the order positional args[0] > config job.
func resolveJob(args []string) (string, error) {
	if len(args) >= 1 && args[0] != "" {
		return args[0], nil
	}
	if cfg.Job != "" {
		return cfg.Job, nil
	}
	if cfg.Service != "" {
		return "", fmt.Errorf("job is required: the config file names a service (service: %s); "+
			"use the top-level commands such as 'clrnd deploy' for it", cfg.Service)
	}
	return "", fmt.Errorf("job is required: pass it as an argument or set job in the config file")
}
