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

// defaultRevisionsKeep は --prune の既定の保持数。消しすぎるより残しすぎる方が安全なので
// 多めに取ってある。
const defaultRevisionsKeep = 10

func runRevisions(cmd *cobra.Command, args []string) error {
	service, err := resolveService(args)
	if err != nil {
		return err
	}
	if err := validateFormat(revisionsFormat); err != nil {
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

// pruneRevisions は古いリビジョンを削除する。消す対象は stdout にデータとして出し
// (--format json でも読める)、確認と結果は stderr に出す。
func pruneRevisions(cmd *cobra.Command, client *cloudrun.Client, revisions cloudrun.Revisions) error {
	targets := cloudrun.SelectPrunableRevisions(revisions, revisionsKeep)
	if len(targets) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "Nothing to prune.")
		return nil
	}

	// 何を消すのかを先に見せる。件数だけでなく中身を出すのは、配信中やタグ付きの
	// リビジョンが混ざっていないことを目で確かめられるようにするため。
	if err := writeFormatted(cmd, revisionsFormat, targets, targets.Text()); err != nil {
		return err
	}
	// --dry-run は何も消さないので確認を求めない (delete と同じ方針)。
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
