package cmd

import (
	"fmt"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/spf13/cobra"
)

var (
	revisionsProject     string
	revisionsRegion      string
	revisionsFormat      string
	revisionsPrune       bool
	revisionsKeep        int
	revisionsAutoApprove bool
	revisionsDryRun      bool
)

var revisionsCmd = &cobra.Command{
	Use:   "revisions [service]",
	Short: "List the revisions of a service",
	Long: "List the revisions of a service, newest first: the name, the Ready condition, the\n" +
		"share of traffic each one currently receives, its traffic tags, when it was created,\n" +
		"and its container images (every container, so a revision with a sidecar lists more than\n" +
		"one). Listing is read-only; nothing is modified.\n" +
		"With --prune, the revisions older than the newest --keep are deleted. Cloud Run keeps\n" +
		"every revision forever otherwise, and a service has a limit on how many it can hold.\n" +
		"A revision serving traffic or carrying a tag is never deleted, however old it is.\n" +
		"service may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(1),
	RunE: runRevisions,
}

func init() {
	addTargetFlags(revisionsCmd, &revisionsProject, &revisionsRegion)
	addFormatFlag(revisionsCmd, &revisionsFormat)
	revisionsCmd.Flags().BoolVar(&revisionsPrune, "prune", false,
		"delete the revisions older than the newest --keep (never one serving traffic or tagged)")
	revisionsCmd.Flags().IntVar(&revisionsKeep, "keep", defaultRevisionsKeep,
		"how many of the newest revisions to keep when pruning")
	revisionsCmd.Flags().BoolVar(&revisionsAutoApprove, "auto-approve", false,
		"prune without the interactive confirmation prompt")
	revisionsCmd.Flags().BoolVar(&revisionsDryRun, "dry-run", false,
		"with --prune, only show what would be deleted")
}

// defaultRevisionsKeep is the default number of revisions --prune keeps. Keeping too many is safer
// than deleting too many, so it is set on the generous side.
const defaultRevisionsKeep = 10

func runRevisions(cmd *cobra.Command, args []string) error {
	service, err := resolveService(args)
	if err != nil {
		return err
	}
	if err := validateFormat(revisionsFormat); err != nil {
		return err
	}
	// Check the consistency of the pruning flags before creating the client (= ADC discovery).
	if err := validatePruneFlags(cmd); err != nil {
		return err
	}

	client, err := newCloudRunClient(cmd, revisionsProject, revisionsRegion)
	if err != nil {
		return err
	}

	revisions, err := client.ListRevisions(cmd.Context(), service)
	if err != nil {
		return err
	}
	if revisionsPrune {
		return pruneRevisions(cmd, client, revisions)
	}
	return writeFormatted(cmd, revisionsFormat, revisions, revisions.Text())
}

// validatePruneFlags validates the combination of pruning flags.
//
// Silently ignoring --keep or --auto-approve given without --prune would let a run that "meant to
// prune but only listed" finish as a success. Clamping a negative --keep to 0 is the same kind of
// accident (deleting everything that is not protected), so it is refused here.
func validatePruneFlags(cmd *cobra.Command) error {
	if !revisionsPrune {
		for _, name := range []string{"keep", "auto-approve", "dry-run"} {
			if cmd.Flags().Changed(name) {
				return fmt.Errorf("--%s only applies with --prune", name)
			}
		}
		return nil
	}
	if revisionsKeep < 0 {
		return fmt.Errorf("--keep must not be negative, got %d", revisionsKeep)
	}
	return nil
}

// pruneRevisions deletes old revisions. What is to be deleted goes to stdout as data (readable with
// --format json too), and the confirmation and results go to stderr.
func pruneRevisions(cmd *cobra.Command, client *cloudrun.Client, revisions cloudrun.Revisions) error {
	targets := cloudrun.SelectPrunableRevisions(revisions, revisionsKeep)
	if targets == nil {
		// Emit [] rather than null in JSON (the same shape as the listing path).
		targets = cloudrun.Revisions{}
	}

	// Show what is about to be deleted first. The entries themselves are printed, not just a
	// count, so the user can see for themselves that no serving or tagged revision is among them.
	// Output is written even when there is nothing to delete: for --format json users, a stdout
	// that is empty only on days with nothing to prune would break uses like `| jq 'length'`.
	if err := writeFormatted(cmd, revisionsFormat, targets, targets.Text()); err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "Nothing to prune.")
		return nil
	}
	// --dry-run deletes nothing, so it does not ask for confirmation (the same policy as delete).
	if revisionsDryRun {
		fmt.Fprintf(cmd.ErrOrStderr(), "Dry run: would delete %d revision(s).\n", len(targets))
		return nil
	}

	ok, err := confirmAction(cmd, revisionsAutoApprove, "prune",
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
