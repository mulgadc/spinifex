package host

import (
	"context"
	"errors"
	"fmt"
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
