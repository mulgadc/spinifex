package mock

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
)

func (m *Client) EnsureTransitGatewayLink(_ context.Context, attachmentID string, ends [2]ovn.TransitGatewayLinkEnd) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if m.EnsureTransitGatewayLinkErr != nil {
		return m.EnsureTransitGatewayLinkErr
	}
	for _, end := range ends {
		if _, ok := m.Routers[end.Router]; !ok {
			return fmt.Errorf("transit gateway link: %w: %q", ovn.ErrLogicalRouterNotFound, end.Router)
		}
	}
	changed := false
	for _, end := range ends {
		lr := m.Routers[end.Router]
		port := end.Port
		existing, ok := m.RouterPorts[port.Name]
		switch {
		case ok && slices.Contains(lr.Ports, existing.UUID) && ovn.SameRouterPort(existing, &port):
		case ok:
			port.UUID = existing.UUID
			m.RouterPorts[port.Name] = &port
			changed = true
		default:
			port.UUID = awsidentifiers.GenerateResourceID("ovn")
			lr.Ports = append(lr.Ports, port.UUID)
			m.RouterPorts[port.Name] = &port
			changed = true
		}
		changed = syncOwned(m.LRPolicies, &lr.Policies, end.Policies, policyStamp, ovn.PolicyKey, attachmentID, "lrp") || changed
		changed = syncOwned(m.StaticRoutes, &lr.StaticRoutes, end.Routes, routeStamp, ovn.StaticRouteKey, attachmentID, "route") || changed
	}
	if changed {
		m.EnsureTransitGatewayLinkWrites++
	}
	return nil
}

// syncOwned makes the rows attached through *attached and stamped owner
// exactly want, matched by key, and reports a change. The caller holds m.Mu.
func syncOwned[T any](rows map[string]*T, attached *[]string, want []T, at func(*T) (*string, map[string]string), key func(*T) string, owner, prefix string) bool {
	wanted := map[string]*T{}
	for i := range want {
		wanted[key(&want[i])] = &want[i]
	}
	changed := false
	kept := map[string]bool{}
	*attached = slices.DeleteFunc(*attached, func(uuid string) bool {
		r, ok := rows[uuid]
		if !ok {
			return false
		}
		if _, ext := at(r); ext[ovn.ExternalIDTGWAttachmentID] != owner || ext[ovn.ExternalIDRole] == ovn.TransitGatewayRerouteRole {
			return false
		}
		k := key(r)
		w, ok := wanted[k]
		if !ok || kept[k] {
			delete(rows, uuid)
			changed = true
			return true
		}
		kept[k] = true
		row := *w
		u, _ := at(&row)
		*u = uuid
		if !reflect.DeepEqual(r, &row) {
			rows[uuid] = &row
			changed = true
		}
		return false
	})
	for k, w := range wanted {
		if kept[k] {
			continue
		}
		row := *w
		u, _ := at(&row)
		*u = awsidentifiers.GenerateResourceID(prefix)
		*attached = append(*attached, *u)
		rows[*u] = &row
		changed = true
	}
	return changed
}

func (m *Client) DeleteTransitGatewayLink(_ context.Context, attachmentID string, routers []string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if m.DeleteTransitGatewayLinkErr != nil {
		return m.DeleteTransitGatewayLinkErr
	}
	m.deleteOwnedLink(ovn.ExternalIDTGWAttachmentID, attachmentID, routers)
	return nil
}
