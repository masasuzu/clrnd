//go:build e2e

package e2e

import (
	"path/filepath"
	"testing"
	"time"

	run "google.golang.org/api/run/v1"
)

const (
	scalingMode         = "run.googleapis.com/scalingMode"
	manualInstanceCount = "run.googleapis.com/manualInstanceCount"
)

func (s *suite) poolState(t *testing.T) *run.WorkerPool {
	t.Helper()
	pool, err := s.getWorkerPool(t, s.ctx)
	if err != nil {
		s.fatal(t, "read worker pool: %v", err)
	}
	return pool
}

// poolReady waits for the worker pool to settle at its current generation, independently of
// clrnd's own wait.
func (s *suite) poolReady(t *testing.T) *run.WorkerPool {
	t.Helper()
	var pool *run.WorkerPool
	s.eventually(t, 3*time.Minute, func() bool {
		pool = s.poolState(t)
		if pool.Status == nil || pool.Status.ObservedGeneration < pool.Metadata.Generation {
			return false
		}
		for _, c := range pool.Status.Conditions {
			if c.Type == "Ready" {
				if c.Status == "False" {
					s.fatal(t, "worker pool rollout failed: %s: %s", c.Reason, c.Message)
				}
				return c.Status == "True"
			}
		}
		return false
	})
	return pool
}

func (s *suite) poolCount(t *testing.T, pool *run.WorkerPool, want string) {
	t.Helper()
	if got := pool.Metadata.Annotations[manualInstanceCount]; got != want {
		s.fatal(t, "worker pool runs %q instances, want %q", got, want)
	}
}

func (s *suite) workerPools(t *testing.T) {
	s.own("worker pool", s.pool)
	dir := s.dir(t, "worker-pools")
	s.config(t, dir)
	manifest := filepath.Join(dir, "pool.yaml")
	s.write(t, manifest, workerPoolManifest(s.pool, s.c.image))

	t.Log("The manifest the API would turn into a stopped worker pool is refused before it is sent")
	stopping := filepath.Join(dir, "stopping.yaml")
	s.write(t, stopping, s.read(t, manifest))
	s.edit(t, stopping, func(m map[string]any) { setAnnotation(m, scalingMode, "manual") })
	r := s.fails(t, s.cli(t, dir, "worker-pools", "verify", s.pool, stopping, "--local-only"))
	s.contains(t, r.stderr, "sets the worker pool to 0 instances")

	r = s.code(t, s.cli(t, dir, "worker-pools", "verify", s.pool, manifest), 0)
	s.empty(t, r.stdout)
	s.lacks(t, r.stderr, "warning:")
	r = s.code(t, s.cli(t, dir, "worker-pools", "deploy", s.pool, manifest, "--auto-approve", "--timeout", "180s"), 0)
	s.contains(t, r.stderr, "worker pool: "+s.pool)
	s.contains(t, r.stderr, "Ready=True")
	pool := s.poolReady(t)
	s.poolCount(t, pool, "1")
	first := pool.Status.LatestReadyRevisionName

	r = s.code(t, s.cli(t, dir, "worker-pools", "status", s.pool), 0)
	for _, want := range []string{"Ready:           True", "Instances:       1 (manual)", "100%  " + first} {
		s.contains(t, r.stdout, want)
	}
	r = s.code(t, s.cli(t, dir, "worker-pools", "status", s.pool, "--format", "json"), 0)
	status := decode[struct {
		WorkerPool    string
		InstanceCount *int
	}](s, t, []byte(r.stdout))
	if status.WorkerPool != s.pool || status.InstanceCount == nil || *status.InstanceCount != 1 {
		s.fatal(t, "unexpected status JSON: %s", r.stdout)
	}
	r = s.code(t, s.cli(t, dir, "worker-pools", "wait", s.pool, "--timeout", "60s"), 0)
	s.empty(t, r.stdout)
	r = s.code(t, s.cli(t, dir, "worker-pools", "diff", s.pool, manifest, "--exit-code"), 0)
	s.empty(t, r.stdout)
	r = s.code(t, s.cli(t, dir, "worker-pools", "diff", s.pool, manifest, "--exit-code", "--no-server-defaults"), 2)
	s.contains(t, r.stdout, manualInstanceCount)
	r = s.fails(t, s.cli(t, dir, "deploy", s.pool, manifest, "--auto-approve"))
	s.contains(t, r.stderr, "use the 'clrnd worker-pools' commands")

	t.Log("A manifest from init diffs clean, and owns the instance count it writes")
	initDir := s.dir(t, "worker-pools-init")
	s.code(t, s.cli(t, initDir, "worker-pools", "init", s.pool, "--project", s.c.project, "--region", s.c.region), 0)
	scaffold := filepath.Join(initDir, "manifest.yaml")
	s.contains(t, string(s.read(t, scaffold)), "kind: WorkerPool")
	s.contains(t, string(s.read(t, filepath.Join(initDir, "clrnd.yml"))), "workerPool: "+s.pool)
	for _, args := range [][]string{{"worker-pools", "diff", "--exit-code"}, {"worker-pools", "diff", "--exit-code", "--no-server-defaults"}} {
		r = s.code(t, s.cli(t, initDir, args...), 0)
		s.empty(t, r.stdout)
	}
	r = s.fails(t, s.cli(t, initDir, "diff"))
	s.contains(t, r.stderr, "names a worker pool")
	s.edit(t, scaffold, func(m map[string]any) { setAnnotation(m, manualInstanceCount, "2") })
	s.code(t, s.cli(t, initDir, "worker-pools", "deploy", "--auto-approve", "--timeout", "180s"), 0)
	pool = s.poolReady(t)
	s.poolCount(t, pool, "2")
	// A count is not part of the revision: changing it alone creates none.
	if pool.Status.LatestCreatedRevisionName != first {
		s.fatal(t, "a count-only change created a revision")
	}

	t.Log("A manifest without a count leaves it to whoever sets it, as an autoscaler does")
	if err := s.scaleWorkerPool(t, "3"); err != nil {
		s.fatal(t, "scale the worker pool: %v", err)
	}
	s.poolReady(t)
	r = s.code(t, s.cli(t, dir, "worker-pools", "diff", s.pool, manifest, "--exit-code"), 0)
	s.empty(t, r.stdout)
	s.edit(t, manifest, func(m map[string]any) { setEnv(m, "second") })
	s.code(t, s.cli(t, dir, "worker-pools", "deploy", s.pool, manifest, "--auto-approve", "--timeout", "180s"), 0)
	pool = s.poolReady(t)
	s.poolCount(t, pool, "3")
	if pool.Status.LatestReadyRevisionName == first {
		s.fatal(t, "a template change created no revision")
	}
	r = s.code(t, s.cli(t, dir, "worker-pools", "diff", s.pool, manifest, "--exit-code"), 0)
	s.empty(t, r.stdout)

	r = s.code(t, s.cli(t, dir, "worker-pools", "delete", s.pool, "--dry-run"), 0)
	s.contains(t, r.stderr, "instances:   3 (stopped with it)")
	s.poolState(t)
	s.code(t, s.cli(t, dir, "worker-pools", "delete", s.pool, "--auto-approve", "--timeout", "180s"), 0)
	if _, err := s.getWorkerPool(t, s.ctx); !isNotFound(err) {
		s.fatal(t, "expected the worker pool to be absent, got error %v", err)
	}
}
