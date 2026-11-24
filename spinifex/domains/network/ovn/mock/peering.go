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

		upsertOwned(m.LRPolicies, &lr.Policies, end.Guard, policyStamp, "lrp")
		upsertOwned(m.LRPolicies, &lr.Policies, end.NoRoute, policyStamp, "lrp")
		upsertOwned(m.StaticRoutes, &lr.StaticRoutes, end.Entry, routeStamp, "route")
	}
	return nil
}

func policyStamp(r *nbdb.LogicalRouterPolicy) (*string, map[string]string) {
	return &r.UUID, r.ExternalIDs
}

func routeStamp(r *nbdb.LogicalRouterStaticRoute) (*string, map[string]string) {
	return &r.UUID, r.ExternalIDs
}

// upsertOwned keeps one row of want's connection and role among the rows
// attached through *attached, rewritten to want. The caller holds m.Mu.
func upsertOwned[T any](rows map[string]*T, attached *[]string, want T, at func(*T) (*string, map[string]string), prefix string) {
	wantUUID, wantExt := at(&want)
	for _, uuid := range slices.Clone(*attached) {
		r, ok := rows[uuid]
		if !ok {
			continue
		}
		if _, ext := at(r); !ovn.SamePeeringOwner(ext, wantExt) {
			continue
		}
		if *wantUUID == "" {
			*wantUUID = uuid
			continue
		}
		delete(rows, uuid)
		*attached = slices.DeleteFunc(*attached, func(u string) bool { return u == uuid })
	}
	if *wantUUID == "" {
		*wantUUID = awsidentifiers.GenerateResourceID(prefix)
		*attached = append(*attached, *wantUUID)
	}
	rows[*wantUUID] = &want
}

// deleteOwned removes the rows whose external ID key is id from rows and from *attached.
func deleteOwned[T any](rows map[string]*T, attached *[]string, at func(*T) (*string, map[string]string), key, id string) {
	*attached = slices.DeleteFunc(*attached, func(uuid string) bool {
		r, ok := rows[uuid]
		if !ok {
			return false
		}
		if _, ext := at(r); ext[key] != id {
			return false
		}
		delete(rows, uuid)
		return true
	})
}

func (m *Client) DeletePeeringLink(_ context.Context, pcxID string, routers []string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if m.DeletePeeringLinkErr != nil {
		return m.DeletePeeringLinkErr
	}
	m.deleteOwnedLink(ovn.ExternalIDPcxID, pcxID, routers)
	return nil
}

// deleteOwnedLink removes every row whose external ID key is id on the named
// routers. The caller holds m.Mu.
func (m *Client) deleteOwnedLink(key, id string, routers []string) {
	for _, name := range routers {
		lr, ok := m.Routers[name]
		if !ok {
			continue
		}
		lr.Ports = slices.DeleteFunc(lr.Ports, func(uuid string) bool {
			for portName, p := range m.RouterPorts {
				if p.UUID == uuid && p.ExternalIDs[key] == id {
					delete(m.RouterPorts, portName)
					return true
				}
			}
			return false
		})
		deleteOwned(m.StaticRoutes, &lr.StaticRoutes, routeStamp, key, id)
		deleteOwned(m.LRPolicies, &lr.Policies, policyStamp, key, id)
	}
}
