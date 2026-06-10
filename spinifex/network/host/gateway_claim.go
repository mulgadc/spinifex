package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// GatewayClaimProber checks and repairs the Southbound chassis claim for OVN
// gateway router ports by shelling out to ovn-sbctl / ovn-appctl. It backs the
// reconcile.GatewayClaimVerifier interface; the compile-time check lives at the
// wiring site (vpcd), since this host-layer package cannot import the reconcile
// cross-cutter without re-tangling the layer tree.
type GatewayClaimProber struct {
	sbAddr string
}

// NewGatewayClaimProber returns a prober that queries the OVN Southbound DB at
// sbAddr (empty uses the local default socket).
func NewGatewayClaimProber(sbAddr string) *GatewayClaimProber {
	return &GatewayClaimProber{sbAddr: sbAddr}
}

// GatewayPortClaimed reports whether the SB Port_Binding for lrpName has a
// non-empty chassis. An unclaimed binding (chassis == []) means ovn-controller
// has not installed flows for the gateway redirect, so the floating IPs behind
// it are unreachable. ctx is part of the verifier contract; ovn-sbctl is a
// short-lived local query and is not cancelled mid-flight.
func (p *GatewayClaimProber) GatewayPortClaimed(_ context.Context, lrpName string) (bool, error) {
	args := []string{"--no-leader-only"}
	if p.sbAddr != "" {
		args = append(args, "--db="+p.sbAddr)
	}
	args = append(args, "--bare", "--columns=chassis", "find", "Port_Binding", "logical_port="+lrpName)
	// Output() not CombinedOutput(): sudo PAM audit noise on stderr would
	// otherwise be read as a non-empty chassis value.
	out, err := utils.SudoCommand("ovn-sbctl", args...).Output()
	if err != nil {
		return false, fmt.Errorf("ovn-sbctl find Port_Binding %s: %w", lrpName, err)
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
