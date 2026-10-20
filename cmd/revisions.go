package cmd

import (
	"fmt"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

// revisionsOptions holds the flags of one revisions command.
type revisionsOptions struct {
	project, region, format string
	prune                   bool
	keep                    int
	autoApprove, dryRun     bool
}

var revisionsCmd = newRevisionsCmd(serviceTarget)

// newRevisionsCmd builds revisions for a target that has revisions.
func newRevisionsCmd(t target) *cobra.Command {
	o := &revisionsOptions{}
	cmd := &cobra.Command{
		Use:   fmt.Sprintf("revisions [%s]", t.arg()),
		Short: fmt.Sprintf("List the revisions of a %s", t.noun),
		Long: fmt.Sprintf("List the revisions of a %[1]s, newest first: the name, the Ready condition,\n"+
			"%[2]s,\n"+
			"when it was created, and its container images (every container, so a revision with a\n"+
			"sidecar lists more than one). Listing is read-only; nothing is modified.\n"+
			"With --prune, the revisions older than the newest --keep are deleted. Cloud Run keeps\n"+
			"every revision up to a limit otherwise.\n"+
			"Never deleted, however old: a revision %[3]s,\n"+
			"and the latest created one, which Cloud Run refuses to delete.\n"+
			"%[4]s may be omitted when set in the config file.", t.noun, t.split.listed, t.split.protected, t.arg()),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runRevisions(cmd, args, t, o) },
	}
	addTargetFlags(cmd, &o.project, &o.region)
	addFormatFlag(cmd, &o.format)
	cmd.Flags().BoolVar(&o.prune, "prune", false,
		fmt.Sprintf("delete the revisions older than the newest --keep (never one %s)", t.split.protected))
	cmd.Flags().IntVar(&o.keep, "keep", defaultRevisionsKeep,
		"how many of the newest revisions to keep when pruning")
	cmd.Flags().BoolVar(&o.autoApprove, "auto-approve", false,
		"prune without the interactive confirmation prompt")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false,
		"with --prune, only show what would be deleted")
	return cmd
}

// defaultRevisionsKeep is the default number of revisions --prune keeps. Keeping too many is safer
// than deleting too many, so it is set on the generous side.
const defaultRevisionsKeep = 10

func runRevisions(cmd *cobra.Command, args []string, t target, o *revisionsOptions) error {
	name, err := t.resolveName(args)
	if err != nil {
		return err
	}
	if err := validateFormat(o.format); err != nil {
		return err
	}
	// Check the consistency of the pruning flags before creating the client (= ADC discovery).
	if err := o.validatePruneFlags(cmd); err != nil {
		return err
	}

	client, err := newCloudRunClient(cmd, o.project, o.region)
	if err != nil {
		return err
	}

	revisions, err := client.ListKindRevisions(cmd.Context(), t.kind, name)
	if err != nil {
		return err
	}
	if o.prune {
		// Pruning deletes, so say where, as delete does.
		printTarget(cmd, "Target:", client.Project(), client.Region(), t.noun, name, nil)
		return pruneRevisions(cmd, client, t, o, revisions)
	}
	return writeFormatted(cmd, o.format, revisions, t.split.table(revisions))
}

// validatePruneFlags validates the combination of pruning flags.
//
// Silently ignoring --keep, --dry-run or --auto-approve given without --prune would let a run that
// "meant to prune but only listed" finish as a success. Clamping a negative --keep to 0 is the same
// kind of accident (deleting everything that is not protected), so it is refused here.
func (o *revisionsOptions) validatePruneFlags(cmd *cobra.Command) error {
	if !o.prune {
		for _, name := range []string{"keep", "auto-approve", "dry-run"} {
			if cmd.Flags().Changed(name) {
				return fmt.Errorf("--%s only applies with --prune", name)
			}
		}
		return nil
	}
	if o.keep < 0 {
		return fmt.Errorf("--keep must not be negative, got %d", o.keep)
	}
	return nil
}

// pruneRevisions deletes old revisions. What is to be deleted goes to stdout as data (readable with
// --format json too), and the confirmation and results go to stderr.
func pruneRevisions(cmd *cobra.Command, client *cloudrun.Client, t target, o *revisionsOptions,
	revisions cloudrun.Revisions) error {
	targets := cloudrun.SelectPrunableRevisions(revisions, o.keep)
	if targets == nil {
		// Emit [] rather than null in JSON (the same shape as the listing path).
		targets = cloudrun.Revisions{}
	}

	// Show what is about to be deleted first. The entries themselves are printed, not just a
	// count, so the user can see for themselves that no serving or tagged revision is among them.
	// Output is written even when there is nothing to delete: for --format json users, a stdout
	// that is empty only on days with nothing to prune would break uses like `| jq 'length'`.
	if err := writeFormatted(cmd, o.format, targets, t.split.table(targets)); err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "Nothing to prune.")
		return nil
	}
	// --dry-run deletes nothing, so it does not ask for confirmation (the same policy as delete).
	if o.dryRun {
		fmt.Fprintf(cmd.ErrOrStderr(), "Dry run: would delete %d revision(s).\n", len(targets))
		return nil
	}

	ok, err := confirmAction(cmd, o.autoApprove, "prune",
		fmt.Sprintf("Delete %d revision(s)? This cannot be undone.", len(targets)))
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	ctx := cmd.Context()
	for _, r := range targets {
		if err := client.DeleteRevision(ctx, r.Name); err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "deleted %s\n", r.Name)
	}
	return nil
}
