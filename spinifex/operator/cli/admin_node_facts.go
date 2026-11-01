package cli

import (
	"github.com/mulgadc/spinifex/spinifex/runtime/host/nodefacts"
	"github.com/spf13/cobra"
)

var adminNodeFactsCmd = &cobra.Command{
	Use:   "node-facts",
	Short: "Record this node's hardware and cabling for the operator console",
}

var adminNodeFactsCollectCmd = &cobra.Command{
	Use:   "collect",
	Short: "Run the root-only hardware probes (run by spinifex-daemon.service as root)",
	Long: `Runs dmidecode, ethtool, udevadm, networkctl lldp and systemd-detect-virt, and
writes their raw output to /etc/spinifex/node-facts/ readable by the spinifex group.
The daemon parses the files on request. A probe that fails leaves no file, and the
console shows that source as unavailable.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return nodefacts.Collect(cmd.Context(), nodefacts.DefaultDir, "/sys")
	},
}

func init() {
	adminCmd.AddCommand(adminNodeFactsCmd)
	adminNodeFactsCmd.AddCommand(adminNodeFactsCollectCmd)
}
