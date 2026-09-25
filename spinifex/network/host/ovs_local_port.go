package host

import (
	"context"
	"fmt"
)

// HasLocalPort reports whether an OVN logical port is bound to an interface on
// this host, which is how a node decides an instance is its own.
//
// The Southbound DB knows this too, but local OVS is the authority on what is
// plugged in here and cannot be wrong about it. portName is the LSP name, which
// is also the value the daemon writes as external_ids:iface-id when it creates
// the tap, so the comparison is exact rather than a prefix guess.
func HasLocalPort(ctx context.Context, r Runner, portName string) (bool, error) {
	if portName == "" {
		return false, fmt.Errorf("HasLocalPort: portName required")
	}
	out, err := r.Run(ctx, "ovs-vsctl", "--format=json", "--columns=name,external_ids", "list", "Interface")
	if err != nil {
		return false, fmt.Errorf("list OVS interfaces: %s: %w", string(out), err)
	}
	rows, err := parseOVSInterfaceRows(out)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if row.externalIDs["iface-id"] == portName {
			return true, nil
		}
	}
	return false, nil
}
