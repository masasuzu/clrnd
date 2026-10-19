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
	if s.resourcesStarted {
		for _, v := range []struct{ kind, name string }{{"services", s.service}, {"jobs", s.job}} {
			r := s.command(t, ctx, s.work, nil, "gcloud", "run", v.kind, "delete", v.name, "--project", s.c.project, "--region", s.c.region, "--quiet")
			if r.code != 0 && !notFound(r) {
				t.Error("resource cleanup failed; use --cleanup-orphans (see redacted log)")
			}
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

func (s *suite) preflight(t *testing.T) {
	t.Helper()
	// Report identities without ever printing ADC contents (which include refresh tokens).
	adc := ""
	for _, v := range s.c.env {
		if x, ok := strings.CutPrefix(v, "GOOGLE_APPLICATION_CREDENTIALS="); ok {
			adc = x
		}
	}
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
		r := s.command(t, s.ctx, s.work, nil, "gcloud", "config", "get-value", "auth/impersonate_service_account")
		if account != "" && strings.TrimSpace(r.stdout) != account {
			s.log(t, "warning: gcloud and clrnd use different identities")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		s.fatal(t, "read ADC: %v", err)
	}
	s.command(t, s.ctx, s.work, nil, "gcloud", "config", "get-value", "account")
	// After the identities, so a permission failure here follows the account it was made as.
	for _, kind := range []string{"services", "jobs"} {
		s.code(t, s.gcloud(t, "run", kind, "list", "--limit", "1", "--format=json"), 0)
	}
}

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
	s := &suite{c: c, ctx: ctx, work: work, service: name, job: name + "-job"}
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
	s.resourcesStarted = true
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
}
