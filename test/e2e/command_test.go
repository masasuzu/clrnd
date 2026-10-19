//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type result struct {
	stdout, stderr string
	code           int
}

func (r result) text() string { return r.stdout + "\n" + r.stderr }

type suite struct {
	resourcesStarted                      bool
	c                                     settings
	ctx                                   context.Context
	work, binary, oldBinary, service, job string
	transcript                            *os.File
}

var (
	urlPattern      = regexp.MustCompile(`https://[A-Za-z0-9._-]*\.run\.app`)
	resourcePattern = regexp.MustCompile(`clrnd-e2e-[0-9]{8,}[A-Za-z0-9-]*`)
	numberPattern   = regexp.MustCompile(`[0-9]{9,}`)
	emailPattern    = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)+`)
)

func redact(text, project string) string {
	text = urlPattern.ReplaceAllString(text, "<service-url>")
	text = resourcePattern.ReplaceAllString(text, "<resource>")
	text = strings.ReplaceAll(text, project, "<project>")
	text = emailPattern.ReplaceAllString(text, "<account>")
	return numberPattern.ReplaceAllString(text, "<number>")
}
func (s *suite) log(t *testing.T, text string) {
	t.Helper()
	if !s.c.raw {
		text = redact(text, s.c.project)
	}
	t.Log(text)
}

// A scratch path must remain usable for inspection; its random numeric suffix is not a cloud ID.
func (s *suite) logWork(t *testing.T, prefix string) {
	t.Helper()
	path := s.work
	if !s.c.raw {
		path = strings.ReplaceAll(path, s.c.project, "<project>")
	}
	t.Log(prefix + path)
}

func (s *suite) fatal(t *testing.T, format string, args ...any) {
	t.Helper()
	text := fmt.Sprintf(format, args...)
	if !s.c.raw {
		text = redact(text, s.c.project)
	}
	t.Fatal(text)
}

func (s *suite) command(t *testing.T, ctx context.Context, dir string, extra map[string]string, bin string, args ...string) result {
	t.Helper()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = environment(s.c.env, extra)
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = strings.NewReader("") // Never inherit a terminal or consume the caller's input.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	r := result{stdout: stdout.String(), stderr: stderr.String()}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			r.code = exit.ExitCode()
		} else {
			r.code = -1
			r.stderr += "\n" + err.Error()
		}
	}
	line := fmt.Sprintf("$ %s %s\nexit=%d\nstdout:\n%sstderr:\n%s", filepath.Base(bin), strings.Join(args, " "), r.code, r.stdout, r.stderr)
	if s.transcript != nil {
		if _, e := fmt.Fprintln(s.transcript, line); e != nil {
			s.fatal(t, "write transcript: %v", e)
		}
	}
	if filepath.Base(bin) == "gcloud" && r.code == 0 {
		s.log(t, fmt.Sprintf("$ gcloud %s (exit=0; full output in commands.log)", strings.Join(args, " ")))
	} else {
		s.log(t, line)
	}
	return r
}
func (s *suite) cli(t *testing.T, dir string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, 15*time.Minute)
	defer cancel()
	return s.command(t, ctx, dir, nil, s.binary, args...)
}
func (s *suite) gcloud(t *testing.T, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Minute)
	defer cancel()
	args = append(args, "--project", s.c.project, "--region", s.c.region)
	return s.command(t, ctx, s.work, nil, "gcloud", args...)
}
func (s *suite) code(t *testing.T, r result, want int) result {
	t.Helper()
	if r.code != want {
		s.fatal(t, "exit=%d, want %d: %s", r.code, want, r.text())
	}
	return r
}
func (s *suite) fails(t *testing.T, r result) result {
	t.Helper()
	if r.code == 0 {
		s.fatal(t, "command unexpectedly succeeded: %s", r.text())
	}
	return r
}
func (s *suite) contains(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		s.fatal(t, "missing %q in %s", want, text)
	}
}
func (s *suite) lacks(t *testing.T, text, unwanted string) {
	t.Helper()
	if strings.Contains(text, unwanted) {
		s.fatal(t, "unexpected %q in %s", unwanted, text)
	}
}
func (s *suite) empty(t *testing.T, text string) {
	t.Helper()
	if text != "" {
		s.fatal(t, "expected empty stdout, got %s", text)
	}
}
func (s *suite) dir(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(s.work, name)
	if err := os.MkdirAll(p, 0o700); err != nil {
		s.fatal(t, "create directory: %v", err)
	}
	return p
}
func (s *suite) write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		s.fatal(t, "write file: %v", err)
	}
}
func (s *suite) read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		s.fatal(t, "read file: %v", err)
	}
	return b
}
func decode[T any](s *suite, t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		s.fatal(t, "decode JSON: %v: %s", err, b)
	}
	return v
}
func (s *suite) config(t *testing.T, dir string) {
	t.Helper()
	s.write(t, filepath.Join(dir, "clrnd.yml"), []byte(fmt.Sprintf("project: %s\nregion: %s\n", s.c.project, s.c.region)))
}
