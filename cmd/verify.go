package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

// verifyOptions are verify's flags. Each verify command (services, jobs) has its own.
type verifyOptions struct {
	project   string
	region    string
	tfstate   []string
	images    []string
	localOnly bool
	format    string
}

// verifyResult is the --format json output. It is the same material as the text output (the
// warnings on stderr and the error on failure) in structured form, so CI can decide for itself
// whether to ignore a warning or fail on it.
type verifyResult struct {
	// Service, Job or WorkerPool names what was verified; the others are left out.
	Service    string `json:"service,omitempty"`
	Job        string `json:"job,omitempty"`
	WorkerPool string `json:"workerPool,omitempty"`
	Manifest   string `json:"manifest"`
	// OK reports whether there was no failure. It is false when anything is Missing or local
	// validation failed.
	OK bool `json:"ok"`
	// Errors are the local validation failures.
	Errors []string `json:"errors,omitempty"`
	// Missing are references confirmed not to exist (failures).
	Missing []string `json:"missing,omitempty"`
	// Unchecked are references that could not be checked (warnings).
	Unchecked []string `json:"unchecked,omitempty"`
	// Warnings are any other advice (a pinned revision name, etc.).
	Warnings []string `json:"warnings,omitempty"`
}

var verifyCmd = newVerifyCmd(serviceTarget)

// newVerifyCmd builds verify for a target.
func newVerifyCmd(t target) *cobra.Command {
	o := &verifyOptions{}
	warnings := "warnings (a check that could not be completed) go to\nstderr"
	if t.split != nil {
		warnings = "warnings (a pinned revision name, a check\n" +
			"that could not be completed) go to stderr"
	}
	cmd := &cobra.Command{
		Use:   fmt.Sprintf("verify [%s] [manifest]", t.arg()),
		Short: "Verify a manifest",
		Long: fmt.Sprintf("Validate that the manifest file is a well-formed Cloud Run %[1]s definition and\n"+
			"contains the fields required to deploy. This local check never needs the API.\n"+
			"When --project/--region are resolvable (and --local-only is not set), it also checks\n"+
			"via the API that referenced resources actually exist: the service account, the Secret\n"+
			"Manager secrets and the versions they reference, the VPC connector and Cloud SQL\n"+
			"instances named in the annotations, and container images hosted on Artifact Registry\n"+
			"(*-docker.pkg.dev).\n"+
			"Images on gcr.io, Docker Hub, or any other registry are not checked and not reported.\n"+
			"--image checks the image you would deploy with, not the one written in the manifest.\n"+
			"A valid manifest produces no output on stdout; %[2]s without failing the command.\n"+
			"--format json prints the result instead — missing, unchecked and warnings as one object\n"+
			"on stdout — while the exit code stays the same.\n"+
			"%[3]s and manifest may be omitted when set in the config file.", t.noun, warnings, t.arg()),
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerify(cmd, args, t, o)
		},
	}
	addTargetFlags(cmd, &o.project, &o.region)
	addManifestFlags(cmd, &o.tfstate)
	addImageFlag(cmd, &o.images)
	cmd.Flags().BoolVar(&o.localOnly, "local-only", false,
		"skip the API existence checks and validate the manifest locally only")
	addFormatFlag(cmd, &o.format)
	return cmd
}

func runVerify(cmd *cobra.Command, args []string, t target, o *verifyOptions) error {
	// Validate the output format first, like the other local checks, and so that every failure
	// after it can be reported in that format.
	if err := validateFormat(o.format); err != nil {
		return err
	}
	// From here on, every failure goes through finishVerify: for --format json users, any path
	// that ends with an empty stdout makes `clrnd verify --format json | jq ...` fail on unreadable
	// output — and a template that does not render (an unset must_env) is the commonest failure
	// in CI.
	var result verifyResult
	fail := func(err error) error {
		result.Errors = append(result.Errors, strings.Split(err.Error(), "\n")...)
		return finishVerify(cmd, o.format, result, err)
	}
	name, err := t.resolveName(args)
	if err != nil {
		return fail(err)
	}
	*t.resultName(&result) = name
	manifestPath, err := resolveManifest(args)
	if err != nil {
		return fail(err)
	}
	result.Manifest = manifestPath
	ctx := cmd.Context()

	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return fail(fmt.Errorf("failed to read manifest %s: %w", manifestPath, err))
	}
	manifest, err = renderManifest(ctx, manifest, o.tfstate)
	if err != nil {
		return fail(err)
	}

	// Apply the same overrides so that what gets verified is what deploy applies (the image
	// existence check also runs against the overridden image).
	manifest, err = cloudrun.ApplyImageOverrides(t.kind, manifest, o.images)
	if err != nil {
		return fail(err)
	}

	// Local schema validation always runs. Validate returns an errors.Join, so split it into one
	// entry per line for the structured form (the text output still prints them together as one
	// error, as before).
	if err := cloudrun.Validate(t.kind, manifest, name); err != nil {
		return fail(err)
	}

	// A pinned revision name is syntactically valid, but the next deploy that changes the template
	// is certain to fail, so warn about it (without failing: for a one-shot deploy it is the right
	// way to write it). A kind without revisions (a job) pins none.
	warning, err := pinnedRevisionWarning(t.kind, manifest)
	if err != nil {
		return fail(err)
	}
	if warning != "" {
		result.Warnings = append(result.Warnings, warning)
	}

	if !o.localOnly {
		if err := verifyRemote(ctx, cmd, t, o, manifest, &result); err != nil {
			return fail(err)
		}
	}

	// Only what is confirmed not to exist is returned as a failure.
	var failure error
	if len(result.Missing) > 0 {
		failure = fmt.Errorf("%s", strings.Join(result.Missing, "\n"))
	}
	return finishVerify(cmd, o.format, result, failure)
}

// verifyRemote runs the API existence checks and adds their results to result. It does nothing
// when the target does not resolve (so offline verification in CI is not broken).
//
// The region is used only to expand a short VPC connector name into a full resource name
// (IAM / Secret Manager / Artifact Registry / Cloud SQL take no region), but the condition for
// "the deploy target is known" requires project and region together: having only one of them is
// more likely a configuration mistake, and doing nothing is safer than silently reaching for the
// production project.
func verifyRemote(ctx context.Context, cmd *cobra.Command, t target, o *verifyOptions, manifest []byte,
	result *verifyResult) error {
	project, region, ok := resolveTargetOptional(o.project, o.region)
	if !ok {
		// When only one of them was given explicitly, say so instead of silently skipping the
		// remote checks.
		if cmd.Flags().Changed("project") || cmd.Flags().Changed("region") {
			result.Warnings = append(result.Warnings,
				"skipping API existence checks: both --project and --region must be set")
		}
		return nil
	}

	res, err := cloudrun.VerifyRemote(ctx, t.kind, project, region, manifest, clientOptions...)
	if err != nil {
		return err
	}
	result.Missing = append(result.Missing, res.Missing...)
	result.Unchecked = append(result.Unchecked, res.Unchecked...)
	return nil
}

// finishVerify prints the result and returns the failure, if any.
//
// text is as before: stdout is empty on success, warnings go to stderr, and a failure is returned
// as an error (cobra prints it to stderr). json prints the result to stdout as a single object and
// still returns a failure as an error — keeping stdout data-only without changing the exit code.
func finishVerify(cmd *cobra.Command, format string, result verifyResult, failure error) error {
	result.OK = failure == nil

	if format == formatJSON {
		if err := writeFormatted(cmd, formatJSON, result, ""); err != nil {
			return err
		}
		return failure
	}

	out := cmd.ErrOrStderr()
	for _, w := range result.Warnings {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
	// What could not be checked (permission denied, API unreachable, etc.) stays a warning and
	// does not fail verify.
	for _, u := range result.Unchecked {
		fmt.Fprintf(out, "warning: could not verify %s\n", u)
	}
	return failure
}
