package cmd

import (
	"github.com/spf13/cobra"
)

var (
	revisionsProject string
	revisionsRegion  string
	revisionsFormat  string
)

var revisionsCmd = &cobra.Command{
	Use:   "revisions [service]",
	Short: "List the revisions of a service",
	Long: "List the revisions of a service, newest first: the name, the Ready condition, the\n" +
		"share of traffic each one currently receives, its traffic tags, when it was created,\n" +
		"and its container image. This is read-only; nothing is modified.\n" +
		"service may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(1),
	RunE: runRevisions,
}

func init() {
	addTargetFlags(revisionsCmd, &revisionsProject, &revisionsRegion)
	addFormatFlag(revisionsCmd, &revisionsFormat)
}

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
	return writeFormatted(cmd, revisionsFormat, revisions, revisions.Text())
}
