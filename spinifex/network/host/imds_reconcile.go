package host

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// ovsIfaceIDPrefix is the "port-" prefix vm.OVSIfaceID prepends to an ENI to form
// its OVS iface-id. Mirrored here (not imported, to avoid an import cycle) to
// recover the full ENI from a port's iface-id.
const ovsIfaceIDPrefix = "port-"

// IMDSTapEndpoint pairs a local primary-ENI's full ID with its br-imds endpoint
// port — the unit vpcd reconciles an IMDS responder against.
type IMDSTapEndpoint struct {
	ENIID    string
	Endpoint string
}

// ListIMDSTaps enumerates the local primary-ENI IMDS datapaths from live OVS state,
// the source vpcd reconciles its responders against. The br-int patch ports ("imi-*")
// carry the OVN iface-id, the only place the full ENI survives on the chassis. A
// malformed iface-id is skipped; an unreadable one aborts the pass so a transient
// read error can't drop a live tap and make reconcile stop its healthy responder.
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
			// Abort rather than drop a live tap: a transient read error would make
			// reconcile treat its healthy responder as stale and stop it.
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
