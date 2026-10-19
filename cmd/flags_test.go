package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"encoding/json"
	"github.com/masasuzu/clrnd/internal/config"
	"github.com/spf13/cobra"
	"google.golang.org/api/googleapi"
	"path/filepath"
	"time"
)

func TestConfirm(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"  yes  \n", true},
		{"n\n", false},
		{"no\n", false},
		{"\n", false}, // empty answer → default No
		{"", false},   // EOF (a pipe, etc.) → No
		{"maybe\n", false},
	}
	for _, tt := range tests {
		t.Run(strings.TrimSpace(tt.input), func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetIn(strings.NewReader(tt.input))
			var errOut bytes.Buffer
			cmd.SetErr(&errOut)

			got, err := confirm(context.Background(), cmd, "Apply?")
			if err != nil {
				t.Fatalf("confirm() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("confirm(%q) = %v, want %v", tt.input, got, tt.want)
			}
			if !strings.Contains(errOut.String(), "Apply? [y/N]:") {
				t.Errorf("prompt not written to stderr: %q", errOut.String())
			}
		})
	}
}

// withConfig temporarily replaces cfg and restores it after the test.
func withConfig(t *testing.T, c *config.Config) {
	t.Helper()
	prev := cfg
	cfg = c
	t.Cleanup(func() { cfg = prev })
}

// withConfigDir temporarily replaces configDir and restores it after the test.
func withConfigDir(t *testing.T, dir string) {
	t.Helper()
	prev := configDir
	configDir = dir
	t.Cleanup(func() { configDir = prev })
}

// blockingReader is a reader whose Read blocks forever. It simulates a terminal waiting for input.
type blockingReader struct{ release chan struct{} }

func (b blockingReader) Read([]byte) (int, error) {
	<-b.release
	return 0, io.EOF
}

// TestConfirmAbortsWhenContextIsCancelled checks that confirm does not stay blocked when ctx is
// cancelled (the equivalent of Ctrl-C) while it is waiting for input.
func TestConfirmAbortsWhenContextIsCancelled(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	cmd := &cobra.Command{}
	cmd.SetIn(blockingReader{release: release})
	var errOut bytes.Buffer
	cmd.SetErr(&errOut)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := confirm(ctx, cmd, "Apply?")
	if got {
		t.Error("confirm() = true, want false when the context is cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("confirm() error = %v, want it to wrap context.Canceled", err)
	}
	if !strings.Contains(errOut.String(), "Apply? [y/N]:") {
		t.Errorf("stderr = %q, want the prompt", errOut.String())
	}
}

func TestResolveConfigPath(t *testing.T) {
	withConfigDir(t, "/etc/app")
	cases := []struct {
		in, want string
	}{
		{"manifest.yaml", "/etc/app/manifest.yaml"}, // relative → against the config dir
		{"sub/m.yaml", "/etc/app/sub/m.yaml"},
		{"/abs/m.yaml", "/abs/m.yaml"},                             // absolute stays as-is
		{"gs://bucket/state.tfstate", "gs://bucket/state.tfstate"}, // a URL stays as-is
		{"", ""},
	}
	for _, c := range cases {
		if got := resolveConfigPath(c.in); got != c.want {
			t.Errorf("resolveConfigPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	t.Run("empty configDir leaves relative path as-is", func(t *testing.T) {
		withConfigDir(t, "")
		if got := resolveConfigPath("manifest.yaml"); got != "manifest.yaml" {
			t.Errorf("got %q, want manifest.yaml", got)
		}
	})
}

func TestResolveManifestUsesConfigDir(t *testing.T) {
	withConfig(t, &config.Config{Manifest: "manifest.yaml"})
	withConfigDir(t, "/etc/app")

	t.Run("config manifest resolved against config dir", func(t *testing.T) {
		got, err := resolveManifest(nil)
		if err != nil || got != "/etc/app/manifest.yaml" {
			t.Fatalf("resolveManifest(nil) = %q, %v; want /etc/app/manifest.yaml", got, err)
		}
	})

	t.Run("positional arg stays cwd-relative", func(t *testing.T) {
		got, err := resolveManifest([]string{"svc", "arg.yaml"})
		if err != nil || got != "arg.yaml" {
			t.Fatalf("resolveManifest(args) = %q, %v; want arg.yaml", got, err)
		}
	})
}

func TestResolveUsesConfigFallback(t *testing.T) {
	t.Run("config used when the flag is empty", func(t *testing.T) {
		clearTargetEnv(t)
		withConfig(t, &config.Config{Project: "cfg-project", Region: "asia-northeast1"})

		got, err := resolveProject("")
		if err != nil || got != "cfg-project" {
			t.Fatalf("resolveProject() = %q, %v; want cfg-project, nil", got, err)
		}
	})

	t.Run("flag wins over config", func(t *testing.T) {
		clearTargetEnv(t)
		withConfig(t, &config.Config{Region: "asia-northeast1"})

		got, err := resolveRegion("us-central1")
		if err != nil || got != "us-central1" {
			t.Fatalf("resolveRegion() = %q, %v; want us-central1, nil", got, err)
		}
	})
}
func TestResolveServiceAndManifest(t *testing.T) {
	t.Run("args take precedence over config", func(t *testing.T) {
		withConfig(t, &config.Config{Service: "cfg-svc", Manifest: "cfg.yaml"})
		svc, err := resolveService([]string{"arg-svc", "arg.yaml"})
		if err != nil || svc != "arg-svc" {
			t.Fatalf("resolveService() = %q, %v", svc, err)
		}
		m, err := resolveManifest([]string{"arg-svc", "arg.yaml"})
		if err != nil || m != "arg.yaml" {
			t.Fatalf("resolveManifest() = %q, %v", m, err)
		}
	})

	t.Run("config used when args absent", func(t *testing.T) {
		withConfig(t, &config.Config{Service: "cfg-svc", Manifest: "cfg.yaml"})
		svc, err := resolveService(nil)
		if err != nil || svc != "cfg-svc" {
			t.Fatalf("resolveService() = %q, %v", svc, err)
		}
		m, err := resolveManifest(nil)
		if err != nil || m != "cfg.yaml" {
			t.Fatalf("resolveManifest() = %q, %v", m, err)
		}
	})

	t.Run("single arg fills service, manifest from config", func(t *testing.T) {
		withConfig(t, &config.Config{Manifest: "cfg.yaml"})
		svc, err := resolveService([]string{"arg-svc"})
		if err != nil || svc != "arg-svc" {
			t.Fatalf("resolveService() = %q, %v", svc, err)
		}
		m, err := resolveManifest([]string{"arg-svc"})
		if err != nil || m != "cfg.yaml" {
			t.Fatalf("resolveManifest() = %q, %v", m, err)
		}
	})

	t.Run("error when neither arg nor config", func(t *testing.T) {
		withConfig(t, &config.Config{})
		if _, err := resolveService(nil); err == nil {
			t.Error("resolveService() = nil error, want error")
		}
		if _, err := resolveManifest(nil); err == nil {
			t.Error("resolveManifest() = nil error, want error")
		}
	})
}

func TestResolveTfstateSources(t *testing.T) {
	t.Run("flag overrides config", func(t *testing.T) {
		withConfig(t, &config.Config{Tfstate: []config.Tfstate{{Location: "gs://cfg/state"}}})
		got, err := resolveTfstateSources([]string{"net=gs://flag/state"})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(got) != 1 || got[0].Name != "net" || got[0].Location != "gs://flag/state" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("config used when no flag", func(t *testing.T) {
		withConfig(t, &config.Config{Tfstate: []config.Tfstate{
			{Location: "gs://cfg/app"},
			{Name: "network", Location: "gs://cfg/net"},
		}})
		got, err := resolveTfstateSources(nil)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(got) != 2 || got[0].Name != "default" || got[1].Name != "network" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("config tfstate without location errors", func(t *testing.T) {
		withConfig(t, &config.Config{Tfstate: []config.Tfstate{{Name: "x"}}})
		if _, err := resolveTfstateSources(nil); err == nil {
			t.Fatal("want error for missing location")
		}
	})
}

func TestParseTfstateSources(t *testing.T) {
	t.Run("location only uses default name", func(t *testing.T) {
		got, err := parseTfstateSources([]string{"terraform.tfstate"})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(got) != 1 || got[0].Name != "default" || got[0].Location != "terraform.tfstate" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("name=location", func(t *testing.T) {
		got, err := parseTfstateSources([]string{"network=gs://bucket/net.tfstate"})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got[0].Name != "network" || got[0].Location != "gs://bucket/net.tfstate" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("hyphen name is not a valid prefix, treated as location", func(t *testing.T) {
		// The name becomes a template function prefix, so it is limited to a Go identifier.
		// A left-hand side containing a hyphen is not taken as a name; the whole value is
		// treated as the location.
		got, err := parseTfstateSources([]string{"my-state=loc"})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got[0].Name != "default" || got[0].Location != "my-state=loc" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("leading-digit name is not a valid prefix, treated as location", func(t *testing.T) {
		got, err := parseTfstateSources([]string{"1state=loc"})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got[0].Name != "default" || got[0].Location != "1state=loc" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("url with = in query is treated as location", func(t *testing.T) {
		got, err := parseTfstateSources([]string{"gs://bucket/path?generation=123"})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got[0].Name != "default" || got[0].Location != "gs://bucket/path?generation=123" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("duplicate name errors", func(t *testing.T) {
		_, err := parseTfstateSources([]string{"a=x", "a=y"})
		if err == nil {
			t.Fatal("want duplicate error")
		}
	})

	t.Run("empty location errors", func(t *testing.T) {
		_, err := parseTfstateSources([]string{"name="})
		if err == nil {
			t.Fatal("want empty location error")
		}
	})
}

// TestResolveProjectIgnoresTheEnvironment checks that the target never comes from the
// environment. CI auth actions and Cloud Shell set these variables to a project nobody chose as the
// deploy target; when they were read, they overrode the config file's project without a word.
func TestResolveProjectIgnoresTheEnvironment(t *testing.T) {
	t.Run("the config wins over a set variable", func(t *testing.T) {
		t.Setenv("CLOUDSDK_CORE_PROJECT", "env-project")
		t.Setenv("GOOGLE_CLOUD_PROJECT", "env-project")
		withConfig(t, &config.Config{Project: "cfg-project"})
		got, err := resolveProject("")
		if err != nil || got != "cfg-project" {
			t.Fatalf("resolveProject() = %q, %v; want cfg-project, nil", got, err)
		}
	})

	t.Run("a set variable alone resolves nothing, and the error says why", func(t *testing.T) {
		clearTargetEnv(t)
		t.Setenv("GOOGLE_CLOUD_PROJECT", "env-project")
		withConfig(t, &config.Config{})
		_, err := resolveProject("")
		if err == nil || !strings.Contains(err.Error(), "project is required") ||
			!strings.Contains(err.Error(), "$GOOGLE_CLOUD_PROJECT is set, but clrnd does not take the target from the environment") {
			t.Fatalf("resolveProject() error = %v, want it to name the ignored variable", err)
		}
	})

	t.Run("nothing set", func(t *testing.T) {
		clearTargetEnv(t)
		withConfig(t, &config.Config{})
		_, err := resolveProject("")
		if err == nil || strings.Contains(err.Error(), "is set") {
			t.Fatalf("resolveProject() error = %v, want a plain 'required' error", err)
		}
	})

	t.Run("surrounding whitespace is trimmed", func(t *testing.T) {
		got, err := resolveProject("  my-project  ")
		if err != nil || got != "my-project" {
			t.Fatalf("resolveProject() = %q, %v; want my-project, nil", got, err)
		}
	})
}
func TestResolveRegionIgnoresTheEnvironment(t *testing.T) {
	t.Run("the config wins over a set variable", func(t *testing.T) {
		t.Setenv("CLOUDSDK_RUN_REGION", "us-central1")
		withConfig(t, &config.Config{Region: "asia-northeast1"})
		got, err := resolveRegion("")
		if err != nil || got != "asia-northeast1" {
			t.Fatalf("resolveRegion() = %q, %v; want asia-northeast1, nil", got, err)
		}
	})

	t.Run("a set variable alone resolves nothing, and the error says why", func(t *testing.T) {
		clearTargetEnv(t)
		t.Setenv("CLOUDSDK_RUN_REGION", "us-central1")
		withConfig(t, &config.Config{})
		_, err := resolveRegion("")
		if err == nil || !strings.Contains(err.Error(), "$CLOUDSDK_RUN_REGION is set") {
			t.Fatalf("resolveRegion() error = %v, want it to name the ignored variable", err)
		}
	})
}

// TestIsInteractiveIsFalseForNonTerminals checks the stdin a CI job typically has. /dev/null is a
// character device, which is what the check used to look for, so it was taken for a terminal.
func TestIsInteractiveIsFalseForNonTerminals(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = devNull.Close() })
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })

	for name, in := range map[string]io.Reader{
		os.DevNull: devNull,
		"a pipe":   r,
		"a reader": strings.NewReader("y\n"),
	} {
		cmd := &cobra.Command{}
		cmd.SetIn(in)
		if isInteractive(cmd) {
			t.Errorf("isInteractive() with %s as stdin = true, want false", name)
		}
	}
}

// TestConfirmActionRefusesWithDevNullAsStdin checks the consequence: with stdin on /dev/null and no
// --auto-approve, the command fails and says why. It used to prompt, read EOF as "no", print
// "Aborted." and succeed, so a CI step that had applied nothing went green.
func TestConfirmActionRefusesWithDevNullAsStdin(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = devNull.Close() })
	cmd := &cobra.Command{}
	cmd.SetIn(devNull)
	var errOut bytes.Buffer
	cmd.SetErr(&errOut)
	cmd.SetContext(context.Background())

	ok, err := confirmAction(cmd, false, "apply", "Apply?")
	if ok || err == nil || !strings.Contains(err.Error(), "refusing to apply without confirmation") {
		t.Errorf("confirmAction() = %v, %v; want a refusal", ok, err)
	}
	if strings.Contains(errOut.String(), "Apply?") {
		t.Errorf("stderr = %q, want no prompt", errOut.String())
	}
}

// TestAMalformedRegionStopsBeforeAnyRequest checks that a region which would move the endpoint's
// host — and with it, where the access token is sent — is refused before anything is sent.
func TestAMalformedRegionStopsBeforeAnyRequest(t *testing.T) {
	requests := 0
	startFakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	})
	for _, region := range []string{"attacker.example/", "asia-northeast1?x", "Asia-Northeast1"} {
		_, _, err := executeRoot(t, "status", "my-svc", "--project", "test-project", "--region", region)
		if err == nil || !strings.Contains(err.Error(), "invalid region") {
			t.Errorf("status --region %q error = %v, want invalid region", region, err)
		}
	}
	if requests != 0 {
		t.Errorf("%d request(s) reached the API, want none", requests)
	}
}

// TestErrorMessageReplacesAnEndpointPage checks the message printed for a 404 that came from a web
// page rather than the API: what to check, instead of the page.
func TestErrorMessageReplacesAnEndpointPage(t *testing.T) {
	page := &googleapi.Error{Code: 404, Body: "<!DOCTYPE html><html>...not found on this server.</html>"}
	got := errorMessage(fmt.Errorf("failed to get service %q: %w", "my-svc", page))
	if strings.Contains(got, "<html>") || !strings.HasPrefix(got, `failed to get service "my-svc": `) ||
		!strings.Contains(got, "check that the region") {
		t.Errorf("errorMessage() = %q", got)
	}
	// Any other error is printed as it is.
	apiErr := fmt.Errorf("failed to get service: %w", &googleapi.Error{Code: 404, Message: "Resource 'x' does not exist."})
	if got := errorMessage(apiErr); got != apiErr.Error() {
		t.Errorf("errorMessage(API 404) = %q, want %q", got, apiErr.Error())
	}
}

func TestReportError(t *testing.T) {
	var b bytes.Buffer
	reportError(&b, fmt.Errorf("failed to get service: %w", errors.New("boom")))
	if b.String() != "Error: failed to get service: boom\n" {
		t.Errorf("reportError() printed %q", b.String())
	}
	// diff --exit-code's differences are a result: exit code 2, and no "Error:" line.
	b.Reset()
	reportError(&b, errDiffFound)
	reportError(&b, nil)
	if b.String() != "" {
		t.Errorf("reportError(errDiffFound / nil) printed %q, want nothing", b.String())
	}
}

func TestValidateWaitFlags(t *testing.T) {
	if err := validateWaitFlags(0, time.Second); err != nil {
		t.Errorf("validateWaitFlags(0, 1s) = %v; 0 means no limit", err)
	}
	if err := validateWaitFlags(-time.Minute, time.Second); err == nil || !strings.Contains(err.Error(), "--timeout must not be negative") {
		t.Errorf("validateWaitFlags(-1m) = %v, want a refusal", err)
	}
	for _, interval := range []time.Duration{0, -time.Second} {
		if err := validateWaitFlags(time.Minute, interval); err == nil || !strings.Contains(err.Error(), "--interval must be positive") {
			t.Errorf("validateWaitFlags(interval %s) = %v, want a refusal", interval, err)
		}
	}
}

// TestWaitFlagsAreCheckedBeforeAnyRequest checks that every command that waits refuses a negative
// --timeout the same way, before building the client. It used to become 10 minutes on some of
// them and an error on others.
func TestWaitFlagsAreCheckedBeforeAnyRequest(t *testing.T) {
	requests := 0
	startFakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	})
	manifest := writeManifest(t, localManifest)
	target := []string{"--project", "test-project", "--region", "asia-northeast1", "--timeout", "-1s"}
	for _, args := range [][]string{
		{"deploy", "my-svc", manifest},
		{"rollback", "my-svc"},
		{"refresh", "my-svc"},
		{"traffic", "my-svc", "--to-latest"},
		{"wait", "my-svc"},
		{"delete", "my-svc"},
		{"jobs", "run", "my-job"},
		{"jobs", "delete", "my-job"},
		{"jobs", "executions", "cancel", "my-job-abcde"},
	} {
		_, _, err := executeRoot(t, append(args, target...)...)
		if err == nil || !strings.Contains(err.Error(), "--timeout must not be negative") {
			t.Errorf("%v --timeout -1s error = %v, want a refusal", args, err)
		}
	}
	if requests != 0 {
		t.Errorf("%d request(s) reached the API, want none", requests)
	}
}

// TestVerifyJSONReportsEveryFailure checks that verify --format json prints its result object even
// when the manifest cannot be read or rendered: an empty stdout breaks `| jq`, and a template with
// an unset must_env is the commonest failure in CI.
func TestVerifyJSONReportsEveryFailure(t *testing.T) {
	clearTargetEnv(t)
	templated := writeManifest(t, strings.Replace(localManifest, "gcr.io/project/image:new",
		`{{ must_env "CLRND_TEST_UNSET_VARIABLE" }}`, 1))
	for name, args := range map[string][]string{
		"render failure":   {"verify", "my-svc", templated},
		"missing manifest": {"verify", "my-svc", filepath.Join(t.TempDir(), "nope.yaml")},
	} {
		stdout, _, err := executeRoot(t, append(args, "--format", "json", "--local-only")...)
		if err == nil {
			t.Errorf("%s: verify succeeded, want a failure", name)
		}
		var got struct {
			OK     bool     `json:"ok"`
			Errors []string `json:"errors"`
		}
		if jerr := json.Unmarshal([]byte(stdout), &got); jerr != nil || got.OK || len(got.Errors) == 0 {
			t.Errorf("%s: stdout = %q (%v), want a result with ok=false and the error", name, stdout, jerr)
		}
	}
}
