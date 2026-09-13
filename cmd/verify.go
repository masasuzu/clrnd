package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

var (
	verifyProject   string
	verifyRegion    string
	verifyTfstate   []string
	verifyImages    []string
	verifyLocalOnly bool
	verifyFormat    string
)

// verifyResult is the --format json output. It is the same material as the text output (the
// warnings on stderr and the error on failure) in structured form, so CI can decide for itself
// whether to ignore a warning or fail on it.
type verifyResult struct {
	Service  string `json:"service"`
	Manifest string `json:"manifest"`
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

var verifyCmd = &cobra.Command{
	Use:   "verify [service] [manifest]",
	Short: "Verify a manifest",
	Long: "Validate that the manifest file is a well-formed Cloud Run service definition and\n" +
		"contains the fields required to deploy. This local check never needs the API.\n" +
		"When --project/--region are resolvable (and --local-only is not set), it also checks\n" +
		"via the API that referenced resources actually exist: the service account, the Secret\n" +
		"Manager secrets and the versions they reference, the VPC connector and Cloud SQL\n" +
		"instances named in the annotations, and container images hosted on Artifact Registry\n" +
		"(*-docker.pkg.dev).\n" +
		"Images on gcr.io, Docker Hub, or any other registry are not checked and not reported.\n" +
		"--image checks the image you would deploy with, not the one written in the manifest.\n" +
		"A valid manifest produces no output on stdout; warnings (a pinned revision name, a check\n" +
		"that could not be completed) go to stderr without failing the command.\n" +
		"--format json prints the result instead — missing, unchecked and warnings as one object\n" +
		"on stdout — while the exit code stays the same.\n" +
		"service and manifest may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(2),
	RunE: runVerify,
}

func init() {
	addTargetFlags(verifyCmd, &verifyProject, &verifyRegion)
	addManifestFlags(verifyCmd, &verifyTfstate)
	addImageFlag(verifyCmd, &verifyImages)
	verifyCmd.Flags().BoolVar(&verifyLocalOnly, "local-only", false,
		"skip the API existence checks and validate the manifest locally only")
	addFormatFlag(verifyCmd, &verifyFormat)
}

func runVerify(cmd *cobra.Command, args []string) error {
	service, err := resolveService(args)
	if err != nil {
		return err
	}
	manifestPath, err := resolveManifest(args)
	if err != nil {
		return err
	}
	// Validate the output format before creating the client, like the other local checks.
	if err := validateFormat(verifyFormat); err != nil {
		return err
	}
	ctx := cmd.Context()

	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to read manifest %s: %w", manifestPath, err)
	}
	manifest, err = renderManifest(ctx, manifest, verifyTfstate)
	if err != nil {
		return err
	}

	result := verifyResult{Service: service, Manifest: manifestPath}

	// Apply the same overrides so that what gets verified is what deploy applies (the image
	// existence check also runs against the overridden image). Every failure from here on goes
	// through finishVerify: for --format json users, any path that ends with an empty stdout makes
	// `clrnd verify --format json | jq ...` fail on unreadable output.
	manifest, err = cloudrun.ApplyImageOverrides(manifest, verifyImages)
	if err != nil {
		result.Errors = strings.Split(err.Error(), "\n")
		return finishVerify(cmd, result, err)
	}

	// Local schema validation always runs. Validate returns an errors.Join, so split it into one
	// entry per line for the structured form (the text output still prints them together as one
	// error, as before).
	if err := cloudrun.Validate(manifest, service); err != nil {
		result.Errors = strings.Split(err.Error(), "\n")
		return finishVerify(cmd, result, err)
	}

	// A pinned revision name is syntactically valid, but the next deploy that changes the template
	// is certain to fail, so warn about it (without failing: for a one-shot deploy it is the right
	// way to write it).
	warning, err := pinnedRevisionWarning(manifest)
	if err != nil {
		return err
	}
	if warning != "" {
		result.Warnings = append(result.Warnings, warning)
	}

	if !verifyLocalOnly {
		if err := verifyRemote(ctx, cmd, manifest, &result); err != nil {
			result.Errors = append(result.Errors, err.Error())
			return finishVerify(cmd, result, err)
		}
	}

	// Only what is confirmed not to exist is returned as a failure.
	var failure error
	if len(result.Missing) > 0 {
		failure = fmt.Errorf("%s", strings.Join(result.Missing, "\n"))
	}
	return finishVerify(cmd, result, failure)
}

// verifyRemote runs the API existence checks and adds their results to result. It does nothing
// when the target does not resolve (so offline verification in CI is not broken).
//
// The region is used only to expand a short VPC connector name into a full resource name
// (IAM / Secret Manager / Artifact Registry / Cloud SQL take no region), but the condition for
// "the deploy target is known" requires project and region together: having only one of them is
// more likely a configuration mistake, and doing nothing is safer than silently reaching for the
// production project.
func verifyRemote(ctx context.Context, cmd *cobra.Command, manifest []byte, result *verifyResult) error {
	project, region, ok := resolveTargetOptional(verifyProject, verifyRegion)
	if !ok {
		// When only one of them was given explicitly, say so instead of silently skipping the
		// remote checks.
		if cmd.Flags().Changed("project") || cmd.Flags().Changed("region") {
			result.Warnings = append(result.Warnings,
				"skipping API existence checks: both --project and --region must be set")
		}
		return nil
	}

	res, err := cloudrun.VerifyRemote(ctx, project, region, manifest, clientOptions...)
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
func finishVerify(cmd *cobra.Command, result verifyResult, failure error) error {
	result.OK = failure == nil

	if verifyFormat == formatJSON {
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
