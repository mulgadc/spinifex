package mock

import (
	"context"
	"fmt"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
)

func (m *Client) EnsurePeeringLink(_ context.Context, ends [2]ovn.PeeringLinkEnd) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.EnsurePeeringLinkCalls++
	if m.EnsurePeeringLinkErr != nil {
		return m.EnsurePeeringLinkErr
	}
	for _, end := range ends {
		if _, ok := m.Routers[end.Router]; !ok {
			return fmt.Errorf("peering link: logical router %q not found", end.Router)
		}
	}
	for _, end := range ends {
		lr := m.Routers[end.Router]
		port := end.Port
		if existing, ok := m.RouterPorts[port.Name]; ok {
			port.UUID = existing.UUID
		} else {
			port.UUID = awsidentifiers.GenerateResourceID("ovn")
			lr.Ports = append(lr.Ports, port.UUID)
		}
		m.RouterPorts[port.Name] = &port

		m.upsertPeeringPolicy(lr, end.Guard)
		m.upsertPeeringPolicy(lr, end.NoRoute)
		m.upsertEntryRoute(lr, end.Entry)
	}
	return nil
}

// upsertPeeringPolicy keeps one policy of want's connection and role on lr,
// rewritten to want. The caller holds m.Mu.
func (m *Client) upsertPeeringPolicy(lr *nbdb.LogicalRouter, want nbdb.LogicalRouterPolicy) {
	pcxID, role := want.ExternalIDs[ovn.ExternalIDPcxID], want.ExternalIDs["spinifex:role"]
	for _, uuid := range slices.Clone(lr.Policies) {
		p, ok := m.LRPolicies[uuid]
		if !ok || p.ExternalIDs[ovn.ExternalIDPcxID] != pcxID || p.ExternalIDs["spinifex:role"] != role {
			continue
		}
		if want.UUID == "" {
			want.UUID = uuid
			continue
		}
		delete(m.LRPolicies, uuid)
		lr.Policies = slices.DeleteFunc(lr.Policies, func(u string) bool { return u == uuid })
	}
	if want.UUID == "" {
		want.UUID = awsidentifiers.GenerateResourceID("lrp")
		lr.Policies = append(lr.Policies, want.UUID)
	}
	m.LRPolicies[want.UUID] = &want
}

// upsertEntryRoute keeps one static route of want's connection on lr,
// rewritten to want. The caller holds m.Mu.
func (m *Client) upsertEntryRoute(lr *nbdb.LogicalRouter, want nbdb.LogicalRouterStaticRoute) {
	pcxID := want.ExternalIDs[ovn.ExternalIDPcxID]
	for _, uuid := range slices.Clone(lr.StaticRoutes) {
		r, ok := m.StaticRoutes[uuid]
		if !ok || r.ExternalIDs[ovn.ExternalIDPcxID] != pcxID {
			continue
		}
		if want.UUID == "" {
			want.UUID = uuid
			continue
		}
		delete(m.StaticRoutes, uuid)
		lr.StaticRoutes = slices.DeleteFunc(lr.StaticRoutes, func(u string) bool { return u == uuid })
	}
	if want.UUID == "" {
		want.UUID = awsidentifiers.GenerateResourceID("route")
		lr.StaticRoutes = append(lr.StaticRoutes, want.UUID)
	}
	m.StaticRoutes[want.UUID] = &want
}

func (m *Client) DeletePeeringLink(_ context.Context, pcxID string, routers []string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	for _, name := range routers {
		lr, ok := m.Routers[name]
		if !ok {
			continue
		}
		lr.Ports = slices.DeleteFunc(lr.Ports, func(uuid string) bool {
			for portName, p := range m.RouterPorts {
				if p.UUID == uuid && p.ExternalIDs[ovn.ExternalIDPcxID] == pcxID {
					delete(m.RouterPorts, portName)
					return true
				}
			}
			return false
		})
		lr.StaticRoutes = slices.DeleteFunc(lr.StaticRoutes, func(uuid string) bool {
			r, ok := m.StaticRoutes[uuid]
			if ok && r.ExternalIDs[ovn.ExternalIDPcxID] == pcxID {
				delete(m.StaticRoutes, uuid)
				return true
			}
			return false
		})
		lr.Policies = slices.DeleteFunc(lr.Policies, func(uuid string) bool {
			p, ok := m.LRPolicies[uuid]
			if ok && p.ExternalIDs[ovn.ExternalIDPcxID] == pcxID {
				delete(m.LRPolicies, uuid)
				return true
			}
			return false
		})
	}
	return nil
}
