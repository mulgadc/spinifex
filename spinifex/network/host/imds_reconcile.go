package host

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// ovsIfaceIDPrefix is the "port-" prefix topology.Port / vm.OVSIfaceID prepend to
// an ENI to form its OVS iface-id. Mirrored here (not imported) to recover the
// full ENI from a port's iface-id — the cross-package name-contract convention
// the IMDS host code already follows (see imdsPortLSPName) to avoid an import cycle.
const ovsIfaceIDPrefix = "port-"

// IMDSTapEndpoint pairs a local primary-ENI's full ID with its br-imds endpoint
// port — the unit vpcd reconciles an IMDS responder against.
type IMDSTapEndpoint struct {
	ENIID    string
	Endpoint string
}

// ListIMDSTaps enumerates the local primary-ENI IMDS datapaths from live OVS
// state, the source of truth vpcd reconciles its responders against. The br-int
// patch ports (IMDSIntPatchPort, "imi-*") carry the OVN iface-id
// (vm.OVSIfaceID(eniID) = "port-<eniID>"), the only place the *full* ENI survives
// on the chassis — the br-imds endpoint name hashes it to 8 hex chars.
// OVS on this chassis holds only local ports, so the result is inherently the
// local tap set. A port with an unexpected (malformed) iface-id is skipped; a
// port whose iface-id cannot be read aborts the pass so the caller retries next
// tick, since a transient read error must not drop a live tap and let reconcile
// stop its healthy responder.
func ListIMDSTaps(ctx context.Context, r Runner) ([]IMDSTapEndpoint, error) {
	out, err := r.Run(ctx, "ovs-vsctl", "list-ports", "br-int")
	if err != nil {
		return nil, fmt.Errorf("list br-int ports: %w", err)
	}
	var taps []IMDSTapEndpoint
	for port := range strings.FieldsSeq(string(out)) {
		if !strings.HasPrefix(port, imdsIntPatchPrefix) {
			continue
		}
		eniID, err := imdsPatchENI(ctx, r, port)
		if err != nil {
			// A read error (ovsdb busy, or the port deleted mid-read by a concurrent
			// terminate) must not drop a live tap from the set: reconcile would treat
			// its healthy responder as stale and stop it. Abort the pass so the next
			// tick retries with every responder intact — a genuinely-gone port simply
			// will not list next time.
			return nil, fmt.Errorf("read iface-id for %s: %w", port, err)
		}
		if eniID == "" {
			slog.Warn("IMDS: skipping IMDS patch port with unexpected iface-id", "port", port)
			continue
		}
		taps = append(taps, IMDSTapEndpoint{ENIID: eniID, Endpoint: IMDSEndpointName(eniID)})
	}
	return taps, nil
}

// imdsPatchENI reads a br-int IMDS patch port's iface-id and recovers the full
// ENI. Returns "" (not an error) when the iface-id is missing the expected
// "port-" prefix, so the caller skips rather than aborts.
func imdsPatchENI(ctx context.Context, r Runner, port string) (string, error) {
	out, err := r.Run(ctx, "ovs-vsctl", "get", "Interface", port, "external_ids:iface-id")
	if err != nil {
		return "", err
	}
	ifaceID := strings.Trim(strings.TrimSpace(string(out)), `"`)
	if !strings.HasPrefix(ifaceID, ovsIfaceIDPrefix) {
		return "", nil
	}
	return strings.TrimPrefix(ifaceID, ovsIfaceIDPrefix), nil
}
