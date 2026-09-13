package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/masasuzu/clrnd/internal/render"
	"github.com/spf13/cobra"
	"google.golang.org/api/option"
)

// Environment variables consulted when the project/region flags are not given (gcloud-compatible).
// Earlier ones take precedence.
const (
	envProjectPrimary   = "CLOUDSDK_CORE_PROJECT" // gcloud config core/project
	envProjectSecondary = "GOOGLE_CLOUD_PROJECT"  // Google client library standard
	envRegionPrimary    = "CLOUDSDK_RUN_REGION"   // gcloud config run/region
	envRegionSecondary  = "GOOGLE_CLOUD_REGION"
)

// addTargetFlags registers the --project / --region flags. They are required, but when not given
// they fall back to environment variables and the config file, so MarkFlagRequired is not used and
// resolve* validates them instead. The usage text names those fallbacks too (so --help explains why
// a command works, or does not, without the flag being passed).
func addTargetFlags(cmd *cobra.Command, project, region *string) {
	cmd.Flags().StringVar(project, "project", "",
		fmt.Sprintf("GCP project ID (env: %s, %s; config: project)", envProjectPrimary, envProjectSecondary))
	cmd.Flags().StringVar(region, "region", "",
		fmt.Sprintf("Cloud Run region, e.g. asia-northeast1 (env: %s, %s; config: region)",
			envRegionPrimary, envRegionSecondary))
}

// Output formats for subcommands that have machine-readable output.
const (
	formatText = "text"
	formatJSON = "json"
)

// addFormatFlag registers --format. init/render already use -o/--output to mean "the output
// file", so the format is chosen with --format, the same as gcloud.
func addFormatFlag(cmd *cobra.Command, format *string) {
	cmd.Flags().StringVar(format, "format", formatText,
		fmt.Sprintf("output format: %s or %s", formatText, formatJSON))
}

// validateFormat validates the value of --format. Call it before creating the client
// (= ADC discovery). In the opposite order a flag mistake is hidden behind an authentication error.
func validateFormat(format string) error {
	if format != formatText && format != formatJSON {
		return fmt.Errorf("invalid --format %q: must be %q or %q", format, formatText, formatJSON)
	}
	return nil
}

// writeFormatted writes to stdout according to --format: value for json, and text as-is for
// text.
func writeFormatted(cmd *cobra.Command, format string, value any, text string) error {
	if format == formatJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(value)
	}
	fmt.Fprint(cmd.OutOrStdout(), text)
	return nil
}

// clientOptions are extra options passed when creating the Cloud Run client. It is normally empty
// and is used only by tests, to inject an httptest fake API.
var clientOptions []option.ClientOption

// newCloudRunClient resolves project/region in the order flag > environment variable > config and
// creates a Cloud Run Admin API client. It is the common entry point for subcommands that call the
// API.
func newCloudRunClient(cmd *cobra.Command, projectFlag, regionFlag string) (*cloudrun.Client, error) {
	project, err := resolveProject(projectFlag)
	if err != nil {
		return nil, err
	}
	region, err := resolveRegion(regionFlag)
	if err != nil {
		return nil, err
	}
	return cloudrun.NewClient(cmd.Context(), project, region, clientOptions...)
}

// resolveService resolves the service in the order positional args[0] > config service.
func resolveService(args []string) (string, error) {
	if len(args) >= 1 && args[0] != "" {
		return args[0], nil
	}
	if cfg.Service != "" {
		return cfg.Service, nil
	}
	return "", fmt.Errorf("service is required: pass it as an argument or set service in the config file")
}

// resolveManifest resolves the manifest in the order positional args[1] > config manifest
// (for subcommands that take both a service and a manifest).
func resolveManifest(args []string) (string, error) {
	return resolveManifestAt(args, 1)
}

// resolveManifestAt resolves the manifest in the order positional args[idx] > config manifest.
// render, which takes no service, uses idx=0 and treats its only positional argument as the
// manifest. A relative path from the config is resolved against the config file's directory.
func resolveManifestAt(args []string, idx int) (string, error) {
	if len(args) > idx && args[idx] != "" {
		return args[idx], nil
	}
	if cfg.Manifest != "" {
		return resolveConfigPath(cfg.Manifest), nil
	}
	return "", fmt.Errorf("manifest is required: pass it as an argument or set manifest in the config file")
}

// resolveConfigPath resolves a relative path written in the config against the config file's
// directory. Absolute paths and URLs with a scheme (gs://, s3://, etc.) are returned unchanged.
func resolveConfigPath(p string) string {
	if p == "" || configDir == "" || filepath.IsAbs(p) || strings.Contains(p, "://") {
		return p
	}
	return filepath.Join(configDir, p)
}

// resolveProject resolves in the order flag > environment variable > config (the same precedence
// as gcloud). It is an error when none of them is set.
func resolveProject(flag string) (string, error) {
	if v := firstNonEmpty(flag, os.Getenv(envProjectPrimary), os.Getenv(envProjectSecondary), cfg.Project); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("project is required: set --project, $%s / $%s, or project in the config file", envProjectPrimary, envProjectSecondary)
}

// resolveRegion resolves in the order flag > environment variable > config. It is an error when
// none of them is set.
func resolveRegion(flag string) (string, error) {
	if v := firstNonEmpty(flag, os.Getenv(envRegionPrimary), os.Getenv(envRegionSecondary), cfg.Region); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("region is required: set --region, $%s / $%s, or region in the config file", envRegionPrimary, envRegionSecondary)
}

// resolveTargetOptional resolves project/region with the same precedence as
// resolveProject/resolveRegion, but when either is missing it returns ok=false instead of an
// error. It is used to run verify's API existence checks "only when a target resolves" (so
// offline verification is not broken).
func resolveTargetOptional(projectFlag, regionFlag string) (project, region string, ok bool) {
	project = firstNonEmpty(projectFlag, os.Getenv(envProjectPrimary), os.Getenv(envProjectSecondary), cfg.Project)
	region = firstNonEmpty(regionFlag, os.Getenv(envRegionPrimary), os.Getenv(envRegionSecondary), cfg.Region)
	if project == "" || region == "" {
		return "", "", false
	}
	return project, region, true
}

// warnPinnedRevision warns on stderr when the manifest pins the revision name
// (spec.template.metadata.name). Cloud Run rejects a revision with the same name but a different
// configuration, so the next deploy that changes the template is certain to fail. verify and
// deploy print the same warning (so that even a CI job that only runs deploy learns the reason
// before the opaque API error).
func warnPinnedRevision(cmd *cobra.Command, manifest []byte) error {
	warning, err := pinnedRevisionWarning(manifest)
	if err != nil || warning == "" {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
	return nil
}

// warnManifestTraffic warns on stderr that --no-traffic overwrites the manifest's spec.traffic.
// It does nothing when the manifest has none.
func warnManifestTraffic(cmd *cobra.Command, manifest []byte) error {
	pinned, err := cloudrun.HasTraffic(manifest)
	if err != nil {
		return err
	}
	if !pinned {
		return nil
	}
	fmt.Fprintln(cmd.ErrOrStderr(),
		"warning: --no-traffic replaces the spec.traffic written in the manifest with the "+
			"split the service is serving now")
	return nil
}

// pinnedRevisionWarning returns the warning text itself (an empty string when nothing is pinned).
// verify's --format json returns warnings in structured form too, so the output destination is
// not hard-wired here.
func pinnedRevisionWarning(manifest []byte) (string, error) {
	revision, err := cloudrun.RevisionName(manifest)
	if err != nil {
		return "", err
	}
	if revision == "" {
		return "", nil
	}
	return fmt.Sprintf(
		"spec.template.metadata.name pins the revision name to %q; "+
			"Cloud Run rejects a revision name that already exists with a different "+
			"configuration, so a later deploy that changes the template will fail", revision), nil
}

// confirm prints the prompt to stderr and reads yes/no from stdin. The default is No.
// A read from stdin cannot be interrupted, so it runs in a goroutine and confirm returns without
// waiting once ctx is cancelled (Ctrl-C, etc.). Otherwise Ctrl-C would not work while the prompt
// is shown.
func confirm(ctx context.Context, cmd *cobra.Command, prompt string) (bool, error) {
	fmt.Fprintf(cmd.ErrOrStderr(), "%s [y/N]: ", prompt)

	type answer struct {
		line string
		err  error
	}
	// When ctx is cancelled this goroutine is left blocked on stdin, but that is not a problem
	// because the process exits right after.
	ch := make(chan answer, 1)
	go func() {
		line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		ch <- answer{line: line, err: err}
	}()

	select {
	case <-ctx.Done():
		// After ^C the cursor is in the middle of the prompt line, so print a newline before
		// returning.
		fmt.Fprintln(cmd.ErrOrStderr())
		return false, fmt.Errorf("aborted: %w", ctx.Err())
	case a := <-ch:
		if a.err != nil && !errors.Is(a.err, io.EOF) {
			return false, a.err
		}
		reply := strings.ToLower(strings.TrimSpace(a.line))
		return reply == "y" || reply == "yes", nil
	}
}

// isInteractive reports whether the command's standard input is a terminal (interactive). It looks
// at the same input source as confirm (cmd.InOrStdin), so the two cannot disagree.
func isInteractive(cmd *cobra.Command) bool {
	f, ok := cmd.InOrStdin().(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// addManifestFlags registers the --tfstate flag. It can be repeated.
func addManifestFlags(cmd *cobra.Command, tfstate *[]string) {
	cmd.Flags().StringArrayVar(tfstate, "tfstate", nil,
		"Terraform state for {{ tfstate }} placeholders: <location> or <name>=<location> "+
			"(<name> becomes the {{ <name>tfstate }} function prefix; repeatable; "+
			"local path or s3://, gs://, ... URL)")
}

// addImageFlag registers --image. Of the commands that read a manifest, it is added only to
// **those involved in applying it** (verify / diff / deploy). render does not get it because render
// is the command that "prints the template expansion as-is", and re-parsing it to apply an
// override would break that property.
func addImageFlag(cmd *cobra.Command, images *[]string) {
	cmd.Flags().StringArrayVar(images, "image", nil,
		"override a container image: <image>, or <container>=<image> when the manifest "+
			"defines more than one container (repeatable)")
}

// renderManifest interprets the tfstate specification (the flag first, otherwise the config) and
// fills in the manifest's placeholders.
func renderManifest(ctx context.Context, manifest []byte, tfstateSpecs []string) ([]byte, error) {
	sources, err := resolveTfstateSources(tfstateSpecs)
	if err != nil {
		return nil, err
	}
	return render.Render(ctx, manifest, sources)
}

// resolveTfstateSources uses the --tfstate flags when given, and the config's tfstate otherwise
// (the flags replace the config).
func resolveTfstateSources(specs []string) ([]render.Source, error) {
	if len(specs) > 0 {
		return parseTfstateSources(specs)
	}
	return configTfstateSources()
}

// configTfstateSources converts the config's tfstate entries into render.Source values.
func configTfstateSources() ([]render.Source, error) {
	var out []render.Source
	seen := make(map[string]bool)
	for _, t := range cfg.Tfstate {
		name := t.Name
		if name == "" {
			name = render.DefaultStateName
		}
		if t.Location == "" {
			return nil, fmt.Errorf("config tfstate %q: location is required", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate tfstate name %q in config", name)
		}
		seen[name] = true
		out = append(out, render.Source{Name: name, Location: resolveConfigPath(t.Location)})
	}
	return out, nil
}

// parseTfstateSources converts each --tfstate specification into a render.Source.
// "name=location" is a named state; a bare "location" is treated as "default".
// Some location URLs contain "=", so a name is taken only when the part before the first "=" has
// the form of a name.
func parseTfstateSources(specs []string) ([]render.Source, error) {
	var out []render.Source
	seen := make(map[string]bool)
	for _, spec := range specs {
		name, loc := render.DefaultStateName, spec
		if i := strings.Index(spec, "="); i > 0 && render.IsValidName(spec[:i]) {
			name, loc = spec[:i], spec[i+1:]
		}
		if loc == "" {
			return nil, fmt.Errorf("invalid --tfstate %q: location is empty", spec)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate --tfstate name %q", name)
		}
		seen[name] = true
		out = append(out, render.Source{Name: name, Location: loc})
	}
	return out, nil
}

// firstNonEmpty returns the first string that is non-empty after trimming surrounding whitespace
// (in its trimmed form). A whitespace-only value is treated as unset and falls back to the next
// source.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
