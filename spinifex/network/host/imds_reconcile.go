package host

import (
	"context"
	"encoding/json"
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
// carry the OVN iface-id, the only place the full ENI survives on the chassis.
//
// One query serves the whole pass: this runs on vpcd's 15s tick, and a per-port
// query was 1+N sudo invocations every tick. The imi- prefix is unambiguous (the
// br-imds patch end is "imp-", the endpoint "ime-"), so filtering every Interface
// on the prefix is equivalent to enumerating br-int and filtering. A failed query
// aborts the pass so a transient OVS error can't drop a live tap and make reconcile
// stop its healthy responder.
func ListIMDSTaps(ctx context.Context, r Runner) ([]IMDSTapEndpoint, error) {
	out, err := r.Run(ctx, "ovs-vsctl", "--format=json", "--columns=name,external_ids", "list", "Interface")
	if err != nil {
		return nil, fmt.Errorf("list OVS interfaces: %w", err)
	}
	rows, err := parseOVSInterfaceRows(out)
	if err != nil {
		return nil, err
	}
	var taps []IMDSTapEndpoint
	for _, row := range rows {
		if !strings.HasPrefix(row.name, imdsIntPatchPrefix) {
			continue
		}
		// Skip rather than abort: the iface-id is the only record of the full ENI, so
		// a port missing it can never be a member of the live set either way, and one
		// structurally broken port must not stall every reconcile pass.
		ifaceID, ok := row.externalIDs["iface-id"]
		if !ok || !strings.HasPrefix(ifaceID, ovsIfaceIDPrefix) {
			slog.Warn("IMDS: skipping IMDS patch port with unexpected iface-id", "port", row.name, "iface_id", ifaceID)
			continue
		}
		eniID := strings.TrimPrefix(ifaceID, ovsIfaceIDPrefix)
		taps = append(taps, IMDSTapEndpoint{ENIID: eniID, Endpoint: IMDSEndpointName(eniID)})
	}
	return taps, nil
}

// ovsInterfaceRow is one interface's name and external_ids from an ovs-vsctl list.
type ovsInterfaceRow struct {
	name        string
	externalIDs map[string]string
}

// ovsListResult is the `ovs-vsctl --format=json list` envelope. Cells stay raw
// because a column's shape follows its schema type: a scalar is a bare JSON
// value, a map is the two-element array ["map", [[k, v], ...]].
type ovsListResult struct {
	Headings []string            `json:"headings"`
	Data     [][]json.RawMessage `json:"data"`
}

// parseOVSInterfaceRows decodes `ovs-vsctl --format=json --columns=name,external_ids
// list Interface`. Column positions come from the headings rather than being
// assumed to follow the --columns order.
func parseOVSInterfaceRows(out []byte) ([]ovsInterfaceRow, error) {
	var result ovsListResult
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parse ovs-vsctl JSON: %w", err)
	}
	nameCol, extCol := -1, -1
	for i, h := range result.Headings {
		switch h {
		case "name":
			nameCol = i
		case "external_ids":
			extCol = i
		}
	}
	if nameCol < 0 || extCol < 0 {
		return nil, fmt.Errorf("ovs-vsctl JSON missing name/external_ids columns, got %v", result.Headings)
	}
	rows := make([]ovsInterfaceRow, 0, len(result.Data))
	for _, cells := range result.Data {
		if nameCol >= len(cells) || extCol >= len(cells) {
			return nil, fmt.Errorf("ovs-vsctl JSON row has %d cells, want at least %d", len(cells), max(nameCol, extCol)+1)
		}
		var name string
		if err := json.Unmarshal(cells[nameCol], &name); err != nil {
			return nil, fmt.Errorf("parse interface name: %w", err)
		}
		ids, err := parseOVSMap(cells[extCol])
		if err != nil {
			return nil, fmt.Errorf("parse external_ids for %s: %w", name, err)
		}
		rows = append(rows, ovsInterfaceRow{name: name, externalIDs: ids})
	}
	return rows, nil
}

// parseOVSMap decodes an OVSDB map cell, serialised as ["map", [[key, value], ...]].
func parseOVSMap(cell json.RawMessage) (map[string]string, error) {
	var wrapper []json.RawMessage
	if err := json.Unmarshal(cell, &wrapper); err != nil {
		return nil, err
	}
	if len(wrapper) != 2 {
		return nil, fmt.Errorf("expected a 2-element map cell, got %d elements", len(wrapper))
	}
	var kind string
	if err := json.Unmarshal(wrapper[0], &kind); err != nil {
		return nil, err
	}
	if kind != "map" {
		return nil, fmt.Errorf("expected a map cell, got %q", kind)
	}
	var pairs [][]string
	if err := json.Unmarshal(wrapper[1], &pairs); err != nil {
		return nil, err
	}
	ids := make(map[string]string, len(pairs))
	for _, p := range pairs {
		if len(p) != 2 {
			return nil, fmt.Errorf("expected a key/value pair, got %d elements", len(p))
		}
		ids[p[0]] = p[1]
	}
	return ids, nil
}
