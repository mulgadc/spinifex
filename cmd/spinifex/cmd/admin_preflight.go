package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/mulgadc/spinifex/spinifex/preflight"
	"github.com/spf13/cobra"
)

var adminPreflightQuiet bool

var adminPreflightCmd = &cobra.Command{
	Use:   "preflight",
	Short: "Verify this host's managed assets against what this build expects",
	Long: `spx knows its own build (spx version) but not whether this host already
has the privileged helper scripts and sudoers grants that build depends on —
setup.sh and setup-ovn.sh install them, and a binary-swap deploy (make
deploy or a manual binary copy) skips those steps.

Checks compare content, not mere existence, so a present-but-stale asset is
caught too. Exits non-zero if any asset is Missing, Stale, or Ungranted.`,
	RunE:         runAdminPreflight,
	SilenceUsage: true,
}

func init() {
	adminCmd.AddCommand(adminPreflightCmd)
	adminPreflightCmd.Flags().BoolVar(&adminPreflightQuiet, "quiet", false, "print only non-OK rows")
}

func runAdminPreflight(_ *cobra.Command, _ []string) error {
	results := preflight.CheckHost()

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PATH\tKIND\tSTATUS\tDETAIL")
	for _, r := range results {
		if adminPreflightQuiet && r.Status == preflight.OK {
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Path, r.Kind, r.Status, r.Detail)
	}
	w.Flush()

	if preflight.HasProblem(results) {
		return fmt.Errorf("host-asset preflight found problems — see table above")
	}
	return nil
}
