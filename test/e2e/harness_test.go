//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const resourcePrefix = "clrnd-e2e-"

func (s *suite) build(t *testing.T, src, dest string) {
	t.Helper()
	r := s.command(t, s.ctx, src, nil, "go", "build", "-o", dest, ".")
	s.code(t, r, 0)
	info, err := os.Stat(dest)
	if err != nil || info.Size() == 0 {
		s.fatal(t, "build did not produce a binary: %v", err)
	}
}

// Cleanup uses a fresh bounded context: an interrupt must stop the test commands, but
// must not prevent deletion. Errors are failures, never mistaken for an absent resource.
func (s *suite) cleanup(t *testing.T) {
	if s.c.keep {
		if s.transcript != nil {
			if err := s.transcript.Close(); err != nil {
				t.Error("close transcript:", err)
			}
		}
		s.logWork(t, "KEEP=1: resources and work directory kept: ")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, v := range s.owned {
		if err := s.deleteResource(t, ctx, v.kind, v.name); err != nil {
			s.errorf(t, "%s cleanup failed; use --cleanup-orphans: %v", v.kind, err)
		}
	}
	if s.transcript != nil {
		if err := s.transcript.Close(); err != nil {
			t.Error("close transcript:", err)
		}
	}
	if t.Failed() {
		s.logWork(t, "work directory kept for inspection: ")
		return
	}
	if err := os.RemoveAll(s.work); err != nil {
		t.Error("remove work directory:", err)
	}
}

// preflight checks gcloud, which pins revisions and cleans up orphans under its own login. Its
// calls are independent, so they run at once rather than paying gcloud's start-up for each.
// ownedResource is a resource a scenario creates, which cleanup deletes.
type ownedResource struct{ kind, name string }

// ownedResources lists what the scenarios c selects create. Cleanup deletes only these: a delete
// sent for a resource no scenario touched needs a permission the run otherwise does not (a run
// with ONLY=jobs would need run.workerpools.delete), and would fail a run whose scenarios passed.
func ownedResources(c settings, service, job, pool string) []ownedResource {
	var out []ownedResource
	if c.only == "" || c.only == "current" || (c.oldRef != "" && c.only == "old") {
		out = append(out, ownedResource{"service", service})
	}
	if c.only == "" || c.only == "jobs" {
		out = append(out, ownedResource{"job", job})
	}
	if c.only == "" || c.only == "worker-pools" {
		out = append(out, ownedResource{"worker pool", pool})
	}
	return out
}

func (s *suite) preflight(t *testing.T) {
	t.Helper()
	calls := [][]string{
		{"config", "get-value", "auth/impersonate_service_account"},
		{"config", "get-value", "account"},
		{"run", "services", "list", "--limit", "1", "--format=json", "--project", s.c.project, "--region", s.c.region},
		{"run", "jobs", "list", "--limit", "1", "--format=json", "--project", s.c.project, "--region", s.c.region},
	}
	results := make([]result, len(calls))
	var wg sync.WaitGroup
	for i, args := range calls {
		wg.Go(func() { results[i] = s.command(t, s.ctx, s.work, nil, "gcloud", args...) })
	}
	wg.Wait()
	// Report identities without ever printing ADC contents (which include refresh tokens).
	adc := adcPath(s.c.env)
	if adc == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			s.fatal(t, "home: %v", err)
		}
		adc = filepath.Join(home, ".config/gcloud/application_default_credentials.json")
	}
	if b, err := os.ReadFile(adc); err == nil {
		var id struct {
			Type  string `json:"type"`
			Email string `json:"client_email"`
			URL   string `json:"service_account_impersonation_url"`
		}
		if err = json.Unmarshal(b, &id); err != nil {
			s.fatal(t, "invalid ADC: %v", err)
		}
		account := id.Email
		if u, err := url.Parse(id.URL); err == nil && id.URL != "" {
			account = strings.TrimSuffix(filepath.Base(u.Path), ":generateAccessToken")
		}
		s.log(t, "clrnd ADC: "+id.Type+" "+account)
		if account != "" && strings.TrimSpace(results[0].stdout) != account {
			s.log(t, "warning: gcloud and clrnd use different identities")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		s.fatal(t, "read ADC: %v", err)
	}
	// After the identities, so a permission failure follows the account it was made as.
	for _, r := range results[2:] {
		s.code(t, r, 0)
	}
}

// cleanupOrphans removes services and jobs with gcloud alone, so it works without ADC. Worker
// pools go through the API, since the gcloud releases still in use predate its worker-pools
// commands: without a usable ADC they are reported as unchecked, after everything else is gone.
func (s *suite) cleanupOrphans(t *testing.T) {
	t.Helper()
	for _, kind := range []string{"services", "jobs"} {
		r := s.code(t, s.gcloud(t, "run", kind, "list", "--format=value(metadata.name)"), 0)
		for _, name := range strings.Fields(r.stdout) {
			if strings.HasPrefix(name, resourcePrefix) {
				s.code(t, s.gcloud(t, "run", kind, "delete", name, "--quiet"), 0)
			}
		}
	}
	var err error
	if s.api, err = newAPI(s.c); err != nil {
		s.errorf(t, "worker pools not checked: the API client needs ADC: %v", err)
		return
	}
	names, err := s.listWorkerPools(t)
	if err != nil {
		s.errorf(t, "worker pools not checked: %v", err)
		return
	}
	for _, name := range names {
		if strings.HasPrefix(name, resourcePrefix) {
			if err := s.deleteResource(t, s.ctx, "worker pool", name); err != nil {
				s.errorf(t, "delete worker pool %s: %v", name, err)
			}
		}
	}
}

// cloudGuard must be set to run TestE2E. The build tag alone does not make a run deliberate: an IDE
// or `go test -tags=e2e ./...` would otherwise create billable resources whenever project.env exists.
const cloudGuard = "CLRND_E2E_CLOUD"

func TestE2E(t *testing.T) {
	if os.Getenv(cloudGuard) != "1" {
		t.Skip("creates billable Cloud Run resources: run test/e2e/run.sh, or set " + cloudGuard + "=1 to run it directly")
	}
	c, err := readSettings()
	if err != nil {
		t.Fatal(err)
	}
	for _, bin := range []string{"go", "gcloud", "git"} {
		if _, err = exec.LookPath(bin); err != nil {
			t.Fatal(err)
		}
	}
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	// Not defer: parallel subtests start only after this function returns, and stop cancels ctx.
	t.Cleanup(stop)
	go func() { <-ctx.Done(); stop() }() // A second interrupt restores the default kill.
	work, err := os.MkdirTemp(c.workRoot, "clrnd-e2e-work.")
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 5)
	if _, err = rand.Read(id); err != nil {
		t.Fatal(err)
	}
	name := resourcePrefix + time.Now().UTC().Format("20060102150405") + "-" + hex.EncodeToString(id)
	// A worker pool may not share a service's name, hence the suffix.
	s := &suite{c: c, ctx: ctx, work: work, service: name, job: name + "-job", pool: name + "-wp"}
	s.binary = filepath.Join(work, "clrnd")
	if runtime.GOOS == "windows" {
		s.binary += ".exe"
	}
	transcript, err := os.OpenFile(filepath.Join(work, "commands.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	s.transcript = transcript
	// Orphan cleanup intentionally operates on a prefix; ordinary cleanup only owns this run's names.
	if c.cleanupOnly {
		defer func() {
			_ = transcript.Close()
			if !t.Failed() {
				_ = os.RemoveAll(work)
			} else {
				s.logWork(t, "work directory kept: ")
			}
		}()
		s.preflight(t)
		s.cleanupOrphans(t)
		return
	}
	t.Cleanup(func() { s.cleanup(t) })
	// After the orphan branch, which uses gcloud alone and must work without ADC.
	if s.api, err = newAPI(c); err != nil {
		t.Fatal(err)
	}
	s.logWork(t, "work directory: ")
	s.preflight(t)
	s.build(t, c.root, s.binary)
	r := s.cli(t, work, "status", resourcePrefix+"preflight-absent", "--project", c.project, "--region", c.region)
	if !notFound(r) {
		s.fatal(t, "clrnd preflight expected an API 404: %s", r.text())
	}
	if c.oldRef != "" {
		src := s.dir(t, "old-src")
		archive := filepath.Join(work, "old.tar")
		f, e := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if e != nil {
			s.fatal(t, "archive: %v", e)
		}
		cmd := exec.CommandContext(ctx, "git", "archive", c.oldRef)
		cmd.Dir = c.root
		cmd.Stdout = f
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err = cmd.Run()
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			s.fatal(t, "git archive: %v %v %s", err, closeErr, stderr.String())
		}
		if err = extractArchive(archive, src); err != nil {
			s.fatal(t, "extract archive: %v", err)
		}
		s.oldBinary = filepath.Join(work, "clrnd-old")
		if runtime.GOOS == "windows" {
			s.oldBinary += ".exe"
		}
		s.build(t, src, s.oldBinary)
	}
	s.owned = ownedResources(c, s.service, s.job, s.pool)
	// The service and the job are separate resources, so their scenarios run in parallel. The
	// comparison reuses the service and stays behind the service scenario. Cleanup registered
	// above runs only after every parallel subtest has finished.
	current := c.only == "" || c.only == "current"
	comparison := c.oldRef != "" && (c.only == "" || c.only == "old")
	if current || comparison {
		t.Run("service", func(t *testing.T) {
			t.Parallel()
			ok := true
			if current {
				ok = t.Run("current", s.services)
			}
			if !comparison {
				return
			}
			if !ok {
				t.Log("comparison skipped: the service scenario failed, so the live service is in an unknown state")
				return
			}
			t.Run("comparison", s.comparison)
		})
	}
	if c.only == "" || c.only == "jobs" {
		t.Run("jobs", func(t *testing.T) {
			t.Parallel()
			s.jobs(t)
		})
	}
	if c.only == "" || c.only == "worker-pools" {
		t.Run("worker-pools", func(t *testing.T) {
			t.Parallel()
			s.workerPools(t)
		})
	}
}
