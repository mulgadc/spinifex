package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// GatewayClaimProber checks the SB chassis claim for OVN gateway router ports.
// Backs reconcile.GatewayClaimVerifier; compile-time check lives in vpcd.
type GatewayClaimProber struct {
	sbAddr string
}

// NewGatewayClaimProber returns a prober that queries the OVN Southbound DB at
// sbAddr (empty uses the local default socket).
func NewGatewayClaimProber(sbAddr string) *GatewayClaimProber {
	return &GatewayClaimProber{sbAddr: sbAddr}
}

// GatewayPortClaimed reports whether the SB Port_Binding for crPortName has a
// non-empty chassis. Uses the chassisredirect (cr-) port — the bare LRP stays
// chassis-less. Unclaimed binding means flows are not installed and EIPs unreachable.
func (p *GatewayClaimProber) GatewayPortClaimed(_ context.Context, crPortName string) (bool, error) {
	args := []string{"--no-leader-only"}
	if p.sbAddr != "" {
		args = append(args, "--db="+p.sbAddr)
	}
	args = append(args, "--bare", "--columns=chassis", "find", "Port_Binding", "logical_port="+crPortName)
	// Output() not CombinedOutput(): sudo PAM noise on stderr would be
	// misread as a non-empty chassis value.
	out, err := utils.SudoCommand("ovn-sbctl", args...).Output()
	if err != nil {
		return false, fmt.Errorf("ovn-sbctl find Port_Binding %s: %w", crPortName, err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// NudgeRecompute asks the local ovn-controller to re-evaluate logical flows via
// the incremental engine, forcing a re-claim of unbound Port_Bindings.
func (p *GatewayClaimProber) NudgeRecompute(_ context.Context) error {
	out, err := utils.SudoCommand("ovn-appctl", "-t", "ovn-controller", "inc-engine/recompute").CombinedOutput()
	if err != nil {
		return fmt.Errorf("ovn-appctl recompute: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// RepairDatapath re-asserts the WAN-glue veth admin state, then forces a recompute.
// A post-reboot boot race (ovs-vswitchd/ovn-controller start after the passive
// network.target, concurrently with systemd-networkd) can leave veth-wan-ovs
// admin-down — br-wan loses carrier and the external datapath is dead, which a flow
// recompute alone cannot revive. Bring both veth ends up (idempotent; skipped when
// the device is absent, e.g. physical-uplink mode), then recompute to also clear any
// stale gateway ofport flows. Best-effort: link errors are logged, not returned.
func (p *GatewayClaimProber) RepairDatapath(ctx context.Context) error {
	for _, dev := range []string{VethOVSEnd, VethLinuxEnd} {
		if !linkExists(dev) {
			continue
		}
		if out, err := utils.SudoCommand("ip", "link", "set", dev, "up").CombinedOutput(); err != nil {
			slog.Warn("gateway claim: veth uplink admin-up failed", "dev", dev, "out", strings.TrimSpace(string(out)), "err", err)
		}
	}
	return p.NudgeRecompute(ctx)
}

// linkExists reports whether a network device is present. Read-only, no sudo.
func linkExists(dev string) bool {
	return exec.Command("ip", "link", "show", dev).Run() == nil
}

// GatewayReachable pings the gateway LRP IP once to confirm the external datapath
// forwards. OVN logical routers answer ICMP echo to their own port IPs natively,
// so this probes host -> br-wan -> br-ext -> localnet -> OVN gateway router with no
// guest or security-group dependency. A failed ping (non-zero exit) reports
// unreachable, not an error; an error is reserved for the inability to run ping.
func (p *GatewayClaimProber) GatewayReachable(ctx context.Context, gwIP string) (bool, error) {
	if gwIP == "" {
		return false, fmt.Errorf("GatewayReachable: gwIP required")
	}
	cmd := exec.CommandContext(ctx, "ping", "-c", "1", "-W", "1", gwIP)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, fmt.Errorf("ping %s: %w", gwIP, err)
	}
	return true, nil
}
