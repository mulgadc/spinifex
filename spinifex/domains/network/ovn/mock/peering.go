package mock

import (
	"context"
	"fmt"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
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

		guard := end.Guard
		pcxID := guard.ExternalIDs[ovn.ExternalIDPcxID]
		for _, uuid := range slices.Clone(lr.Policies) {
			p, ok := m.LRPolicies[uuid]
			if !ok {
				continue
			}
			switch {
			case p.Priority == guard.Priority && p.Match == guard.Match:
				guard.UUID = uuid
			case p.ExternalIDs[ovn.ExternalIDPcxID] == pcxID && p.ExternalIDs["spinifex:role"] == ovn.PeeringGuardRole:
				delete(m.LRPolicies, uuid)
				lr.Policies = slices.DeleteFunc(lr.Policies, func(u string) bool { return u == uuid })
			}
		}
		if guard.UUID == "" {
			guard.UUID = awsidentifiers.GenerateResourceID("lrp")
			lr.Policies = append(lr.Policies, guard.UUID)
		}
		m.LRPolicies[guard.UUID] = &guard
	}
	return nil
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
