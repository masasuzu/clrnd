//go:build e2e

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

type executionRow struct{ Name, Status string }

func (s *suite) execution(t *testing.T, r result) string {
	t.Helper()
	name := strings.TrimSpace(r.stdout)
	if strings.ContainsAny(name, " \t\r\n") || !strings.HasPrefix(name, s.job+"-") {
		s.fatal(t, "stdout is not an execution name: %s", r.stdout)
	}
	return name
}
func (s *suite) executionStatus(t *testing.T, dir, name, want string) {
	t.Helper()
	r := s.code(t, s.cli(t, dir, "jobs", "executions", "list", "--format", "json"), 0)
	rows := decode[[]executionRow](s, t, []byte(r.stdout))
	for _, e := range rows {
		if e.Name == name {
			if e.Status != want {
				s.fatal(t, "execution %s is %s, want %s", name, e.Status, want)
			}
			return
		}
	}
	s.fatal(t, "execution %s missing from list", name)
}
func (s *suite) jobs(t *testing.T) {
	s.own("job", s.job)
	dir := s.dir(t, "jobs")
	s.config(t, dir)
	manifest := filepath.Join(dir, "job.yaml")
	s.write(t, manifest, jobManifest(s.job, s.c.jobImage))
	s.code(t, s.cli(t, dir, "jobs", "verify", s.job, manifest), 0)
	s.code(t, s.cli(t, dir, "jobs", "deploy", s.job, manifest, "--auto-approve"), 0)
	r := s.code(t, s.cli(t, dir, "jobs", "status", s.job), 0)
	s.contains(t, r.stdout, "Ready:           True")
	s.contains(t, r.stdout, "Executions:      0")
	r = s.code(t, s.cli(t, dir, "jobs", "wait", s.job, "--timeout", "60s"), 0)
	s.empty(t, r.stdout)
	s.contains(t, r.stderr, "Ready=True")
	if job := s.jobState(t); job.Spec.Template.Spec.Template.Spec.MaxRetries != 0 {
		s.fatal(t, "explicit maxRetries: 0 was not kept")
	}
	r = s.code(t, s.cli(t, dir, "jobs", "diff", s.job, manifest, "--exit-code"), 0)
	s.empty(t, r.stdout)
	s.code(t, s.cli(t, dir, "jobs", "diff", s.job, manifest, "--exit-code", "--no-server-defaults"), 2)
	r = s.fails(t, s.cli(t, dir, "deploy", s.job, manifest, "--auto-approve"))
	s.contains(t, r.stderr, "use the 'clrnd jobs' commands")
	serviceFile := filepath.Join(dir, "service.yaml")
	s.write(t, serviceFile, serviceManifest(s.service, s.c.image))
	r = s.fails(t, s.cli(t, dir, "jobs", "deploy", s.service, serviceFile, "--auto-approve"))
	s.contains(t, r.stderr, "use the top-level commands")
	initDir := s.dir(t, "jobs-init")
	s.code(t, s.cli(t, initDir, "jobs", "init", s.job, "--project", s.c.project, "--region", s.c.region), 0)
	scaffold := filepath.Join(initDir, "manifest.yaml")
	text := string(s.read(t, scaffold))
	s.contains(t, text, "kind: Job")
	s.contains(t, text, "maxRetries: 0")
	s.lacks(t, text, "lastUpdatedTime")
	config := string(s.read(t, filepath.Join(initDir, "clrnd.yml")))
	s.contains(t, config, "job: "+s.job)
	s.lacks(t, config, "service:")
	for _, args := range [][]string{{"jobs", "diff", "--exit-code"}, {"jobs", "diff", "--exit-code", "--no-server-defaults"}} {
		r = s.code(t, s.cli(t, initDir, args...), 0)
		s.empty(t, r.stdout)
	}
	r = s.fails(t, s.cli(t, initDir, "diff"))
	s.contains(t, r.stderr, "names a job")
	s.edit(t, scaffold, func(m map[string]any) { setEnv(m, "changed") })
	s.code(t, s.cli(t, initDir, "jobs", "diff", "--exit-code"), 2)
	s.code(t, s.cli(t, initDir, "jobs", "deploy", "--auto-approve"), 0)
	r = s.code(t, s.cli(t, initDir, "jobs", "diff", "--exit-code"), 0)
	s.empty(t, r.stdout)
	r = s.code(t, s.cli(t, initDir, "jobs", "run"), 0)
	s.contains(t, r.stderr, "Completed=True")
	succeeded := s.execution(t, r)
	r = s.code(t, s.cli(t, initDir, "jobs", "status"), 0)
	s.contains(t, r.stdout, "Last execution:  "+succeeded)
	s.contains(t, r.stdout, "Succeeded")
	r = s.code(t, s.cli(t, initDir, "jobs", "run", "--no-wait"), 0)
	noWait := s.execution(t, r)
	r = s.code(t, s.cli(t, initDir, "jobs", "executions", "wait", noWait, "--timeout", "120s"), 0)
	s.empty(t, r.stdout)
	s.contains(t, r.stderr, "Completed=True")
	r = s.fails(t, s.cli(t, initDir, "jobs", "execute", "--env", "FAIL_RATE=1"))
	s.contains(t, r.stderr, "did not succeed")
	failed := s.execution(t, r)
	r = s.fails(t, s.cli(t, initDir, "jobs", "run", "--env", "SLEEP_MS=600000", "--timeout", "10s"))
	s.contains(t, r.stderr, "keeps running")
	s.contains(t, r.stderr, "clrnd jobs executions cancel")
	running := s.execution(t, r)
	r = s.code(t, s.cli(t, initDir, "jobs", "run", "--no-wait", "--env", "SLEEP_MS=600000"), 0)
	leftRunning := s.execution(t, r)
	r = s.code(t, s.cli(t, initDir, "jobs", "executions", "list"), 0)
	for _, entry := range []struct{ name, status string }{{succeeded, "Succeeded"}, {failed, "Failed"}, {running, "Running"}} {
		found := false
		for _, line := range strings.Split(r.stdout, "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == entry.name && fields[1] == entry.status {
				found = true
			}
		}
		if !found {
			s.fatal(t, "text list lacks %s %s", entry.name, entry.status)
		}
	}
	s.executionStatus(t, initDir, running, "Running")
	r = s.fails(t, s.cli(t, initDir, "jobs", "executions", "cancel", succeeded, "--auto-approve"))
	s.contains(t, r.stderr, "has already finished (Succeeded)")
	r = s.fails(t, s.cli(t, initDir, "jobs", "executions", "cancel", running))
	s.contains(t, r.stderr, "refusing to cancel without confirmation")
	s.executionStatus(t, initDir, running, "Running")
	r = s.code(t, s.cli(t, initDir, "jobs", "executions", "cancel", running, "--auto-approve", "--timeout", "120s"), 0)
	s.empty(t, r.stdout)
	s.contains(t, r.stderr, "About to cancel:")
	s.contains(t, r.stderr, "Completed=False (Cancelled)")
	s.executionStatus(t, initDir, running, "Cancelled")
	s.fails(t, s.cli(t, initDir, "jobs", "executions", "list", s.job+"-missing"))
	r = s.code(t, s.cli(t, initDir, "jobs", "delete", "--dry-run"), 0)
	s.contains(t, r.stderr, "deleted with the job")
	s.jobState(t)
	// Check the precondition explicitly: deletion must also stop this execution.
	s.executionStatus(t, initDir, leftRunning, "Running")
	s.code(t, s.cli(t, initDir, "jobs", "delete", "--auto-approve", "--timeout", "120s"), 0)
	s.absent(t, "job")
}
