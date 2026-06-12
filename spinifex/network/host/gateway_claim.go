package host

import (
	"context"
	"fmt"
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
