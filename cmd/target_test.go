package cmd

import (
	"testing"

	"github.com/masasuzu/clrnd/internal/config"
)

// TestTargetsAreComplete checks that every target fills in what the shared commands read from it,
// so adding a kind cannot leave one of them to fail at run time.
func TestTargetsAreComplete(t *testing.T) {
	keys := map[string]bool{}
	for _, tg := range targets {
		if tg.kind == nil || tg.noun == "" || tg.configKey == "" || tg.configName == nil || tg.commands == "" ||
			tg.waitFor == "" || tg.deployLong == "" || tg.manifestFormat == "" || tg.deleteGoes == "" ||
			tg.deleteDetails == nil || tg.resultName == nil {
			t.Errorf("target %q leaves a field empty: %+v", tg.noun, tg)
			continue
		}
		if keys[tg.configKey] {
			t.Errorf("config key %q is used by more than one target", tg.configKey)
		}
		keys[tg.configKey] = true

		// configName and resultName point at the field for this target, and at no other's.
		c := config.Config{}
		*tg.configName(&c) = "x"
		for _, other := range targets {
			if got := *other.configName(&c); (other.configKey == tg.configKey) != (got == "x") {
				t.Errorf("%s's configName and %s's overlap", tg.noun, other.noun)
			}
		}
		r := verifyResult{}
		*tg.resultName(&r) = "x"
		set := 0
		for _, field := range []string{r.Service, r.Job, r.WorkerPool} {
			if field == "x" {
				set++
			}
		}
		if set != 1 {
			t.Errorf("%s's resultName = %+v, want exactly one field set", tg.noun, r)
		}
	}
}

// TestResolveNameRefusesAnotherKindsConfig checks the message every target gives when the config
// names a resource of another kind: the name, and where its commands are.
func TestResolveNameRefusesAnotherKindsConfig(t *testing.T) {
	withConfig(t, &config.Config{Job: "my-job"})
	if _, err := serviceTarget.resolveName(nil); err == nil || err.Error() != "service is required: the config file "+
		"names a job (job: my-job); use the 'clrnd jobs' commands for it" {
		t.Errorf("service resolveName() error = %v", err)
	}
	withConfig(t, &config.Config{Service: "my-svc"})
	if _, err := jobTarget.resolveName(nil); err == nil || err.Error() != "job is required: the config file "+
		"names a service (service: my-svc); use the top-level commands such as 'clrnd deploy' for it" {
		t.Errorf("job resolveName() error = %v", err)
	}
	withConfig(t, &config.Config{WorkerPool: "my-pool"})
	if _, err := serviceTarget.resolveName(nil); err == nil || err.Error() != "service is required: the config file "+
		"names a worker pool (workerPool: my-pool); use the 'clrnd worker-pools' commands for it" {
		t.Errorf("service resolveName() error = %v", err)
	}
	withConfig(t, &config.Config{Job: "my-job"})
	if _, err := workerPoolTarget.resolveName(nil); err == nil || err.Error() != "worker pool is required: the "+
		"config file names a job (job: my-job); use the 'clrnd jobs' commands for it" {
		t.Errorf("worker pool resolveName() error = %v", err)
	}
	withConfig(t, &config.Config{})
	if _, err := jobTarget.resolveName(nil); err == nil || err.Error() != "job is required: pass it as an argument "+
		"or set job in the config file" {
		t.Errorf("job resolveName() with nothing set error = %v", err)
	}
}
