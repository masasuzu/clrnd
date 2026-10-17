package cmd

import (
	"fmt"
	"os"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

// diffOptions are diff's flags. Each diff command (services, jobs) has its own.
type diffOptions struct {
	project    string
	region     string
	tfstate    []string
	images     []string
	noDefaults bool
	exitCode   bool
}

var diffCmd = newDiffCmd(serviceTarget)

// newDiffCmd builds diff for a target.
func newDiffCmd(t target) *cobra.Command {
	o := &diffOptions{}
	cmd := &cobra.Command{
		Use:   fmt.Sprintf("diff [%s] [manifest]", t.noun),
		Short: fmt.Sprintf("Show the diff between an existing %s and a manifest", t.noun),
		Long: fmt.Sprintf("Fetch the live definition of the %[1]s from Cloud Run and show a unified diff\n"+
			"against the given manifest file. Both sides are normalized (read-only fields removed)\n"+
			"before comparison. A %[1]s that does not exist yet shows the whole manifest as added.\n"+
			"Cloud Run fills in a lot of fields on its own, so by default diff asks it to resolve them\n"+
			"first (via a dry run) and compares against that; pass --no-server-defaults to compare\n"+
			"against the manifest as written, which also avoids needing update permission.\n"+
			"Nothing is printed when there is no difference, and diff succeeds either way unless\n"+
			"--exit-code is given.\n"+
			"%[1]s and manifest may be omitted when set in the config file.", t.noun),
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiff(cmd, args, t, o)
		},
	}
	addTargetFlags(cmd, &o.project, &o.region)
	addManifestFlags(cmd, &o.tfstate)
	addImageFlag(cmd, &o.images)
	addServerDefaultsFlag(cmd, &o.noDefaults)
	cmd.Flags().BoolVar(&o.exitCode, "exit-code", false,
		"exit with 2 when there is a difference (0 when there is none, 1 on error)")
	return cmd
}

func runDiff(cmd *cobra.Command, args []string, t target, o *diffOptions) error {
	name, err := t.resolveName(args)
	if err != nil {
		return err
	}
	manifestPath, err := resolveManifest(args)
	if err != nil {
		return err
	}

	ctx := cmd.Context()

	local, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to read manifest %s: %w", manifestPath, err)
	}
	local, err = renderManifest(ctx, local, o.tfstate)
	if err != nil {
		return err
	}
	// Apply the same overrides as deploy. Without this, what diff shows and what deploy applies
	// would differ.
	local, err = cloudrun.ApplyImageOverrides(t.kind, local, o.images)
	if err != nil {
		return err
	}
	// Parse locally before creating the client (= ADC discovery), so that a manifest problem is
	// not hidden behind an authentication error. CompareManifest does the same parse, but it is a
	// pure check and cheap.
	if err := cloudrun.CheckSyntax(t.kind, local); err != nil {
		return err
	}

	client, err := newCloudRunClient(cmd, o.project, o.region)
	if err != nil {
		return err
	}

	out, err := client.CompareManifest(ctx, t.kind, name, local, manifestPath,
		cloudrun.PlanOptions{ResolveDefaults: !o.noDefaults})
	if err != nil {
		return err
	}

	fmt.Fprint(cmd.OutOrStdout(), out)
	if o.exitCode && out != "" {
		return errDiffFound
	}
	return nil
}
