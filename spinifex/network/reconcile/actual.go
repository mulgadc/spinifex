package reconcile

import (
	"context"
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
)

// ActualState is the live OVN NB DB snapshot the reconciler diffs IntentState
// against. Membership in each set is presence-by-name — the apply stage
// drives a Get-then-mutate against the live client when it needs the
// per-row payload.
//
// Orphan deletion is restricted to security group port groups (sg_*) to
// match the legacy ReconcileSGsOnce contract; other resource types are
// create-or-skip only. Tightening this is a Phase 4 cleanup item.
type ActualState struct {
	Routers      map[string]struct{} // logical router name → present
	Switches     map[string]struct{} // logical switch name → present
	Ports        map[string]struct{} // logical switch port name → present
	RouterPorts  map[string]struct{} // logical router port name → present
	PortGroups   map[string]struct{} // port group name → present
	ExternalSwch map[string]struct{} // vpcID → external switch present (spinifex:role=external)
}

func newActualState() ActualState {
	return ActualState{
		Routers:      make(map[string]struct{}),
		Switches:     make(map[string]struct{}),
		Ports:        make(map[string]struct{}),
		RouterPorts:  make(map[string]struct{}),
		PortGroups:   make(map[string]struct{}),
		ExternalSwch: make(map[string]struct{}),
	}
}

// scanActual walks the OVN NB DB once and returns a presence map per
// resource type. Any List failure is fatal because a partial scan produces
// unsafe diff decisions (a missing entry would look like a create
// candidate). Callers must surface the error and skip the reconcile cycle.
func scanActual(ctx context.Context, client ovn.Client) (ActualState, error) {
	actual := newActualState()

	routers, err := client.ListLogicalRouters(ctx)
	if err != nil {
		return ActualState{}, fmt.Errorf("list logical routers: %w", err)
	}
	for _, r := range routers {
		actual.Routers[r.Name] = struct{}{}
	}

	switches, err := client.ListLogicalSwitches(ctx)
	if err != nil {
		return ActualState{}, fmt.Errorf("list logical switches: %w", err)
	}
	for _, s := range switches {
		actual.Switches[s.Name] = struct{}{}
		if s.ExternalIDs["spinifex:role"] == "external" {
			if vpcID := s.ExternalIDs["spinifex:vpc_id"]; vpcID != "" {
				actual.ExternalSwch[vpcID] = struct{}{}
			}
		}
		for _, port := range s.Ports {
			actual.Ports[port] = struct{}{}
		}
	}

	routerPorts, err := client.ListLogicalRouterPorts(ctx)
	if err != nil {
		return ActualState{}, fmt.Errorf("list logical router ports: %w", err)
	}
	for _, rp := range routerPorts {
		actual.RouterPorts[rp.Name] = struct{}{}
	}

	portGroups, err := client.ListPortGroups(ctx)
	if err != nil {
		return ActualState{}, fmt.Errorf("list port groups: %w", err)
	}
	for _, pg := range portGroups {
		actual.PortGroups[pg.Name] = struct{}{}
	}

	return actual, nil
}

// portGroupIsManaged reports whether a port group name is one we own
// (sg_*) and is therefore in scope for orphan deletion. Non-managed
// port groups (third-party usage) are left alone.
func portGroupIsManaged(name string) bool {
	return strings.HasPrefix(name, "sg_")
}
