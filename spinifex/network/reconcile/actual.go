package reconcile

import (
	"context"
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
)

// ActualState is the live OVN NB snapshot used to diff against IntentState.
// Orphan deletion is restricted to SG port groups (sg_*); other types are
// create-or-skip only.
type ActualState struct {
	Routers      map[string]struct{}
	Switches     map[string]struct{}
	Ports        map[string]struct{}
	RouterPorts  map[string]struct{}
	PortGroups   map[string]struct{}
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

// scanActual walks OVN NB once. Any List failure is fatal: a partial scan
// would make missing entries look like create candidates.
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

// portGroupIsManaged reports whether the PG is in scope for orphan deletion
// (sg_* only).
func portGroupIsManaged(name string) bool {
	return strings.HasPrefix(name, "sg_")
}
