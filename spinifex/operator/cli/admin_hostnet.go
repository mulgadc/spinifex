package cli

import (
	"github.com/mulgadc/spinifex/spinifex/runtime/host/hostnet"
	"github.com/spf13/cobra"
)

var adminHostnetCmd = &cobra.Command{
	Use:   "hostnet",
	Short: "Re-create or check this node's host network wiring",
	Long: `On nodes whose host network wiring is not persisted by systemd-networkd,
setup-ovn.sh records it in /etc/spinifex/hostnet.json and spinifex-hostnet.service
re-creates it at boot and whenever ovs-vswitchd restarts.`,
}

var adminHostnetApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Make the host network match the recorded wiring (run by spinifex-hostnet.service)",
	Long: `Creates the uplink veth pairs and sets the links, addresses and encap route
named in /etc/spinifex/hostnet.json. It never deletes a link, and never touches a
link the file does not name. Does nothing on a node whose wiring systemd-networkd
persists. Writes /run/spinifex/hostnet-applied only on success.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return hostnet.Apply(cmd.Context(), hostnet.DefaultEnv())
	},
}

var adminHostnetCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Fail unless the host network is ready for spinifex-daemon and spinifex-vpcd",
	Long: `Runs the same backend and state-file checks as apply, and on the hostnet backend
also needs the marker apply writes on success. The daemon and vpcd run it before
they start, so neither starts on a node whose wiring failed to apply.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return hostnet.Check(cmd.Context(), hostnet.DefaultEnv())
	},
}

func init() {
	adminCmd.AddCommand(adminHostnetCmd)
	adminHostnetCmd.AddCommand(adminHostnetApplyCmd)
	adminHostnetCmd.AddCommand(adminHostnetCheckCmd)
}
