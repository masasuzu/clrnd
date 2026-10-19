//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	run "google.golang.org/api/run/v1"
)

func (s *suite) serviceState(t *testing.T) *run.Service {
	t.Helper()
	svc, err := s.getService(t, s.ctx)
	if err != nil {
		s.fatal(t, "read service: %v", err)
	}
	return svc
}
func (s *suite) jobState(t *testing.T) *run.Job {
	t.Helper()
	job, err := s.getJob(t, s.ctx)
	if err != nil {
		s.fatal(t, "read job: %v", err)
	}
	return job
}
func (s *suite) serving(t *testing.T) string {
	t.Helper()
	svc := s.serviceState(t)
	best := ""
	var percent int64 = -1
	for _, v := range svc.Status.Traffic {
		if v.Percent > percent {
			best = v.RevisionName
			percent = v.Percent
		}
	}
	if best == "" {
		s.fatal(t, "no serving revision")
	}
	return best
}
func (s *suite) percent(t *testing.T, name string, want int64) {
	t.Helper()
	var total int64
	for _, v := range s.serviceState(t).Status.Traffic {
		if v.RevisionName == name {
			total += v.Percent
		}
	}
	if total != want {
		s.fatal(t, "revision %s has %d%% traffic, want %d%%", name, total, want)
	}
}
func (s *suite) revisionCount(t *testing.T) int {
	t.Helper()
	revisions, err := s.listRevisions(t)
	if err != nil {
		s.fatal(t, "list revisions: %v", err)
	}
	return len(revisions)
}

// revisionExists treats only a 404 as absent; any other error stops the scenario.
func (s *suite) revisionExists(t *testing.T, name string) bool {
	t.Helper()
	_, err := s.getRevision(t, name)
	if isNotFound(err) {
		return false
	}
	if err != nil {
		s.fatal(t, "read revision: %v", err)
	}
	return true
}

// notFound reads clrnd's own report of the API's 404. A process that never exited (-1: not
// started, killed by a timeout) proves nothing, and a loose "not found" would also match errors
// such as a missing executable.
func notFound(r result) bool {
	return r.code > 0 && strings.Contains(r.stderr, "googleapi: Error 404")
}
func (s *suite) absent(t *testing.T, kind string) {
	t.Helper()
	var err error
	if kind == "job" {
		_, err = s.getJob(t, s.ctx)
	} else {
		_, err = s.getService(t, s.ctx)
	}
	if !isNotFound(err) {
		s.fatal(t, "expected the %s to be absent, got error %v", kind, err)
	}
}
func (s *suite) eventually(t *testing.T, timeout time.Duration, check func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()
	for {
		if check() {
			return
		}
		select {
		case <-ctx.Done():
			s.fatal(t, "state did not converge: %v", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}
func (s *suite) ready(t *testing.T) {
	t.Helper()
	s.eventually(t, 3*time.Minute, func() bool {
		svc := s.serviceState(t)
		if svc.Status.ObservedGeneration < svc.Metadata.Generation {
			return false
		}
		for _, c := range svc.Status.Conditions {
			if c.Type == "Ready" {
				if c.Status == "False" {
					s.fatal(t, "rollout failed: %s: %s", c.Reason, c.Message)
				}
				return c.Status == "True"
			}
		}
		return false
	})
}
func (s *suite) pinLive(t *testing.T, suffix string) {
	t.Helper()
	s.code(t, s.gcloud(t, "run", "deploy", s.service, "--image", s.c.image, "--revision-suffix", suffix, "--no-allow-unauthenticated", "--quiet"), 0)
	s.ready(t)
}
