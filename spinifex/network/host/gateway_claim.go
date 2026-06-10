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

// GatewayPortClaim reports whether the SB Port_Binding for lrpName looks claimed
// by ovn-controller, and returns the raw row (logical_port/type/chassis/up/
// requested_chassis) for diagnosis. claimed is keyed off a non-empty chassis
// column; the detail string is logged by the caller so the true
// claimed-vs-forwarding signal for this gateway shape can be confirmed from a
// live run before the predicate is hardened. ctx is part of the verifier
// contract; ovn-sbctl is a short-lived local query and is not cancelled
// mid-flight.
func (p *GatewayClaimProber) GatewayPortClaim(_ context.Context, lrpName string) (claimed bool, detail string, err error) {
	// Output() not CombinedOutput(): sudo PAM audit noise on stderr would
	// otherwise pollute the parsed row.
	out, err := utils.SudoCommand("ovn-sbctl", gatewayClaimArgs(p.sbAddr, lrpName)...).Output()
	if err != nil {
		return false, "", fmt.Errorf("ovn-sbctl find Port_Binding %s: %w", lrpName, err)
	}
	detail = strings.TrimSpace(string(out))
	return chassisClaimed(detail), detail, nil
}

// gatewayClaimArgs builds the ovn-sbctl argv that reads the gateway port's SB
// Port_Binding row. sbAddr empty targets the local default socket.
func gatewayClaimArgs(sbAddr, lrpName string) []string {
	args := []string{"--no-leader-only"}
	if sbAddr != "" {
		args = append(args, "--db="+sbAddr)
	}
	return append(args, "--columns=logical_port,type,chassis,up,requested_chassis",
		"find", "Port_Binding", "logical_port="+lrpName)
}

// chassisClaimed parses ovn-sbctl --columns output ("key : value" lines) and
// reports whether the chassis column is a non-empty set.
func chassisClaimed(row string) bool {
	for line := range strings.SplitSeq(row, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "chassis" {
			continue
		}
		val = strings.TrimSpace(val)
		return val != "" && val != "[]"
	}
	return false
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
