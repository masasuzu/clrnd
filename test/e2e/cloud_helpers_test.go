//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	run "google.golang.org/api/run/v1"
)

func (s *suite) serviceState(t *testing.T) *run.Service {
	t.Helper()
	r := s.code(t, s.gcloud(t, "run", "services", "describe", s.service, "--format=json"), 0)
	return decode[*run.Service](s, t, []byte(r.stdout))
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
	r := s.code(t, s.gcloud(t, "run", "revisions", "list", "--service", s.service, "--format=json"), 0)
	return len(decode[[]json.RawMessage](s, t, []byte(r.stdout)))
}

// notFound accepts only the API's and gcloud's own 404 wording. A process that never exited (-1:
// not started, killed by a timeout) proves nothing, and a loose "not found" would also match
// errors such as a missing executable, turning a failed deletion check into a pass.
func notFound(r result) bool {
	if r.code <= 0 {
		return false
	}
	text := r.text()
	if strings.Contains(text, "NOT_FOUND") || strings.Contains(text, "does not exist") || strings.Contains(text, "Error 404") {
		return true
	}
	// gcloud uses resource-specific messages instead of an HTTP status for these calls.
	for _, kind := range []string{"service", "job", "revision"} {
		if strings.Contains(text, "Cannot find "+kind+" [") {
			return true
		}
	}
	return strings.Contains(text, "could not be found.") &&
		(strings.Contains(text, "Service [") || strings.Contains(text, "Job [") || strings.Contains(text, "Revision ["))
}
func (s *suite) absent(t *testing.T, kind, name string) {
	t.Helper()
	r := s.gcloud(t, "run", kind, "describe", name, "--format=json")
	if !notFound(r) {
		s.fatal(t, "expected %s %s to be absent: %s", kind, name, r.text())
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
		case <-time.After(3 * time.Second):
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
