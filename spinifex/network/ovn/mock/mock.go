// Package mock provides an in-memory implementation of ovn.Client for tests.
package mock

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// Client implements ovn.Client with in-memory storage for testing.
type Client struct {
	Mu        sync.Mutex
	connected bool

	Switches       map[string]*nbdb.LogicalSwitch
	Ports          map[string]*nbdb.LogicalSwitchPort
	Routers        map[string]*nbdb.LogicalRouter
	RouterPorts    map[string]*nbdb.LogicalRouterPort
	DHCPOpts       map[string]*nbdb.DHCPOptions
	NATs           map[string]*nbdb.NAT                      // keyed by UUID
	StaticRoutes   map[string]*nbdb.LogicalRouterStaticRoute // keyed by UUID
	PortGroups     map[string]*nbdb.PortGroup                // keyed by name
	ACLs           map[string]*nbdb.ACL                      // keyed by UUID
	GatewayChassis map[string]*nbdb.GatewayChassis           // keyed by UUID

	// UpdateLogicalSwitchPortCalls counts UpdateLogicalSwitchPort invocations
	// so tests can assert idempotent read-before-write paths.
	UpdateLogicalSwitchPortCalls int

	// UpdateLogicalRouterPortCalls counts UpdateLogicalRouterPort invocations
	// so tests can assert writers only emit on drift.
	UpdateLogicalRouterPortCalls int

	// SetGatewayChassisCalls / DeleteGatewayChassisCalls /
	// UpdateGatewayChassisPriorityCalls let tests distinguish between
	// "no-op", "create", "delete", and "priority update" paths through
	// reconcileGatewayChassis (mulga-999).
	SetGatewayChassisCalls            int
	DeleteGatewayChassisCalls         int
	UpdateGatewayChassisPriorityCalls int

	// AddACLErrAfter, when > 0, makes AddACL return an error on the Nth
	// call (1-indexed). Lets fail-fast tests inject a transient OVN error
	// at a specific point in the create/update flow without rewiring the
	// whole client. Counter persists across calls until the mock is
	// recreated.
	AddACLErrAfter int
	AddACLCalls    int
}

var _ ovn.Client = (*Client)(nil)

// New creates a new mock Client for testing.
func New() *Client {
	return &Client{
		Switches:       make(map[string]*nbdb.LogicalSwitch),
		Ports:          make(map[string]*nbdb.LogicalSwitchPort),
		Routers:        make(map[string]*nbdb.LogicalRouter),
		RouterPorts:    make(map[string]*nbdb.LogicalRouterPort),
		DHCPOpts:       make(map[string]*nbdb.DHCPOptions),
		NATs:           make(map[string]*nbdb.NAT),
		StaticRoutes:   make(map[string]*nbdb.LogicalRouterStaticRoute),
		PortGroups:     make(map[string]*nbdb.PortGroup),
		ACLs:           make(map[string]*nbdb.ACL),
		GatewayChassis: make(map[string]*nbdb.GatewayChassis),
	}
}

func (m *Client) Connect(_ context.Context) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.connected = true
	return nil
}

func (m *Client) Close() {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.connected = false
}

func (m *Client) Connected() bool {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	return m.connected
}

// Logical Switch

func (m *Client) CreateLogicalSwitch(_ context.Context, ls *nbdb.LogicalSwitch) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if _, exists := m.Switches[ls.Name]; exists {
		return fmt.Errorf("logical switch %q already exists", ls.Name)
	}
	if ls.UUID == "" {
		ls.UUID = utils.GenerateResourceID("ovn")
	}
	stored := *ls
	m.Switches[ls.Name] = &stored
	return nil
}

// EnsureLogicalSwitch mirrors the live client's wait-op semantics under the
// mock's single mutex: any concurrent caller observing absence will be
// serialised here, so the second arrival sees the row and returns existing.
func (m *Client) EnsureLogicalSwitch(_ context.Context, ls *nbdb.LogicalSwitch) (*nbdb.LogicalSwitch, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if existing, ok := m.Switches[ls.Name]; ok {
		result := *existing
		return &result, nil
	}
	if ls.UUID == "" {
		ls.UUID = utils.GenerateResourceID("ovn")
	}
	stored := *ls
	m.Switches[ls.Name] = &stored
	result := stored
	return &result, nil
}

func (m *Client) DeleteLogicalSwitch(_ context.Context, name string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if _, exists := m.Switches[name]; !exists {
		return fmt.Errorf("logical switch %q not found", name)
	}
	delete(m.Switches, name)
	return nil
}

func (m *Client) GetLogicalSwitch(_ context.Context, name string) (*nbdb.LogicalSwitch, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	ls, exists := m.Switches[name]
	if !exists {
		return nil, fmt.Errorf("logical switch %q not found", name)
	}
	result := *ls
	return &result, nil
}

func (m *Client) ListLogicalSwitches(_ context.Context) ([]nbdb.LogicalSwitch, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	result := make([]nbdb.LogicalSwitch, 0, len(m.Switches))
	for _, ls := range m.Switches {
		result = append(result, *ls)
	}
	return result, nil
}

// Logical Switch Port

func (m *Client) CreateLogicalSwitchPort(_ context.Context, switchName string, lsp *nbdb.LogicalSwitchPort) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	ls, exists := m.Switches[switchName]
	if !exists {
		return fmt.Errorf("logical switch %q not found", switchName)
	}
	if _, exists := m.Ports[lsp.Name]; exists {
		return fmt.Errorf("logical switch port %q already exists", lsp.Name)
	}
	if lsp.UUID == "" {
		lsp.UUID = utils.GenerateResourceID("ovn")
	}
	stored := *lsp
	m.Ports[lsp.Name] = &stored
	ls.Ports = append(ls.Ports, lsp.UUID)
	return nil
}

// CreateLogicalSwitchPortInGroups mirrors the live client's atomic create +
// port-group join path. The mock is not transactional, but every step still
// has to succeed up front; on a port-group lookup failure we leave no LSP
// behind so tests observe the same all-or-nothing semantics as production.
// SB Address_Set rows for `<pg>_ip4` / `<pg>_ip6` are auto-derived by
// ovn-northd in production and intentionally not modelled in the mock.
func (m *Client) CreateLogicalSwitchPortInGroups(_ context.Context, switchName string, lsp *nbdb.LogicalSwitchPort, portGroupNames []string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	ls, exists := m.Switches[switchName]
	if !exists {
		return fmt.Errorf("logical switch %q not found", switchName)
	}
	if _, exists := m.Ports[lsp.Name]; exists {
		return fmt.Errorf("logical switch port %q already exists", lsp.Name)
	}
	for _, pgName := range portGroupNames {
		if _, ok := m.PortGroups[pgName]; !ok {
			return fmt.Errorf("port group %q not found", pgName)
		}
	}
	if lsp.UUID == "" {
		lsp.UUID = utils.GenerateResourceID("ovn")
	}
	stored := *lsp
	m.Ports[lsp.Name] = &stored
	ls.Ports = append(ls.Ports, lsp.UUID)
	for _, pgName := range portGroupNames {
		pg := m.PortGroups[pgName]
		if !slices.Contains(pg.Ports, lsp.UUID) {
			pg.Ports = append(pg.Ports, lsp.UUID)
		}
	}
	return nil
}

func (m *Client) DeleteLogicalSwitchPort(_ context.Context, switchName string, portName string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	port, exists := m.Ports[portName]
	if !exists {
		return fmt.Errorf("logical switch port %q not found", portName)
	}
	ls, exists := m.Switches[switchName]
	if !exists {
		return fmt.Errorf("logical switch %q not found", switchName)
	}
	// Mirror libovsdb's reference-integrity rejection: an LSP still in any
	// port group's Ports set cannot be deleted. Forces every delete-port
	// path to clear membership first — handleDeletePort already does this
	// via reconcilePortSGs, but locking the invariant in the mock catches
	// any reordering that would only break against real OVN.
	for pgName, pg := range m.PortGroups {
		if slices.Contains(pg.Ports, port.UUID) {
			return fmt.Errorf("logical switch port %q still in port group %q", portName, pgName)
		}
	}
	// Remove port UUID from switch's ports list
	for i, uuid := range ls.Ports {
		if uuid == port.UUID {
			ls.Ports = append(ls.Ports[:i], ls.Ports[i+1:]...)
			break
		}
	}
	delete(m.Ports, portName)
	return nil
}

func (m *Client) GetLogicalSwitchPort(_ context.Context, name string) (*nbdb.LogicalSwitchPort, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	lsp, exists := m.Ports[name]
	if !exists {
		return nil, fmt.Errorf("logical switch port %q not found", name)
	}
	result := *lsp
	return &result, nil
}

func (m *Client) UpdateLogicalSwitchPort(_ context.Context, lsp *nbdb.LogicalSwitchPort) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if _, exists := m.Ports[lsp.Name]; !exists {
		return fmt.Errorf("logical switch port %q not found", lsp.Name)
	}
	stored := *lsp
	m.Ports[lsp.Name] = &stored
	m.UpdateLogicalSwitchPortCalls++
	return nil
}

// Logical Router

func (m *Client) CreateLogicalRouter(_ context.Context, lr *nbdb.LogicalRouter) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if _, exists := m.Routers[lr.Name]; exists {
		return fmt.Errorf("logical router %q already exists", lr.Name)
	}
	if lr.UUID == "" {
		lr.UUID = utils.GenerateResourceID("ovn")
	}
	stored := *lr
	m.Routers[lr.Name] = &stored
	return nil
}

// EnsureLogicalRouter mirrors the live client's wait-op semantics under the
// mock's single mutex: concurrent callers observing absence are serialised by
// the mutex, so the second arrival sees the row and returns the existing one.
func (m *Client) EnsureLogicalRouter(_ context.Context, lr *nbdb.LogicalRouter) (*nbdb.LogicalRouter, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if existing, ok := m.Routers[lr.Name]; ok {
		result := *existing
		return &result, nil
	}
	if lr.UUID == "" {
		lr.UUID = utils.GenerateResourceID("ovn")
	}
	stored := *lr
	m.Routers[lr.Name] = &stored
	result := stored
	return &result, nil
}

func (m *Client) DeleteLogicalRouter(_ context.Context, name string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if _, exists := m.Routers[name]; !exists {
		return fmt.Errorf("logical router %q not found", name)
	}
	delete(m.Routers, name)
	return nil
}

func (m *Client) GetLogicalRouter(_ context.Context, name string) (*nbdb.LogicalRouter, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	lr, exists := m.Routers[name]
	if !exists {
		return nil, fmt.Errorf("logical router %q not found", name)
	}
	result := *lr
	return &result, nil
}

func (m *Client) ListLogicalRouters(_ context.Context) ([]nbdb.LogicalRouter, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	result := make([]nbdb.LogicalRouter, 0, len(m.Routers))
	for _, lr := range m.Routers {
		result = append(result, *lr)
	}
	return result, nil
}

func (m *Client) UpdateLogicalRouterExternalIDs(_ context.Context, name string, externalIDs map[string]string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	lr, ok := m.Routers[name]
	if !ok {
		return fmt.Errorf("logical router %q not found", name)
	}
	lr.ExternalIDs = make(map[string]string, len(externalIDs))
	maps.Copy(lr.ExternalIDs, externalIDs)
	return nil
}

// Logical Router Port

func (m *Client) CreateLogicalRouterPort(_ context.Context, routerName string, lrp *nbdb.LogicalRouterPort) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	lr, exists := m.Routers[routerName]
	if !exists {
		return fmt.Errorf("logical router %q not found", routerName)
	}
	if _, exists := m.RouterPorts[lrp.Name]; exists {
		return fmt.Errorf("logical router port %q already exists", lrp.Name)
	}
	if lrp.UUID == "" {
		lrp.UUID = utils.GenerateResourceID("ovn")
	}
	stored := *lrp
	m.RouterPorts[lrp.Name] = &stored
	lr.Ports = append(lr.Ports, lrp.UUID)
	return nil
}

func (m *Client) DeleteLogicalRouterPort(_ context.Context, routerName string, portName string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	port, exists := m.RouterPorts[portName]
	if !exists {
		return fmt.Errorf("logical router port %q not found", portName)
	}
	lr, exists := m.Routers[routerName]
	if !exists {
		return fmt.Errorf("logical router %q not found", routerName)
	}
	for i, uuid := range lr.Ports {
		if uuid == port.UUID {
			lr.Ports = append(lr.Ports[:i], lr.Ports[i+1:]...)
			break
		}
	}
	delete(m.RouterPorts, portName)
	return nil
}

func (m *Client) GetLogicalRouterPort(_ context.Context, name string) (*nbdb.LogicalRouterPort, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	lrp, exists := m.RouterPorts[name]
	if !exists {
		return nil, fmt.Errorf("logical router port %q not found", name)
	}
	result := *lrp
	return &result, nil
}

func (m *Client) UpdateLogicalRouterPort(_ context.Context, lrp *nbdb.LogicalRouterPort) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if _, exists := m.RouterPorts[lrp.Name]; !exists {
		return fmt.Errorf("logical router port %q not found", lrp.Name)
	}
	stored := *lrp
	m.RouterPorts[lrp.Name] = &stored
	m.UpdateLogicalRouterPortCalls++
	return nil
}

// DHCP Options

func (m *Client) CreateDHCPOptions(_ context.Context, opts *nbdb.DHCPOptions) (string, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if opts.UUID == "" {
		opts.UUID = utils.GenerateResourceID("dhcp")
	}
	stored := *opts
	m.DHCPOpts[opts.UUID] = &stored
	return opts.UUID, nil
}

func (m *Client) DeleteDHCPOptions(_ context.Context, uuid string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if _, exists := m.DHCPOpts[uuid]; !exists {
		return fmt.Errorf("DHCP options %q not found", uuid)
	}
	delete(m.DHCPOpts, uuid)
	return nil
}

func (m *Client) FindDHCPOptionsByCIDR(_ context.Context, cidr string) (*nbdb.DHCPOptions, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	for _, opts := range m.DHCPOpts {
		if opts.CIDR == cidr {
			result := *opts
			return &result, nil
		}
	}
	return nil, fmt.Errorf("DHCP options for CIDR %q not found", cidr)
}

func (m *Client) FindDHCPOptionsByExternalID(_ context.Context, key, value string) (*nbdb.DHCPOptions, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	for _, opts := range m.DHCPOpts {
		if opts.ExternalIDs[key] == value {
			result := *opts
			return &result, nil
		}
	}
	return nil, fmt.Errorf("DHCP options with external_id %s=%s not found", key, value)
}

func (m *Client) ListDHCPOptions(_ context.Context) ([]nbdb.DHCPOptions, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	result := make([]nbdb.DHCPOptions, 0, len(m.DHCPOpts))
	for _, opts := range m.DHCPOpts {
		result = append(result, *opts)
	}
	return result, nil
}

// NAT

func (m *Client) AddNAT(_ context.Context, routerName string, nat *nbdb.NAT) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	lr, exists := m.Routers[routerName]
	if !exists {
		return fmt.Errorf("logical router %q not found", routerName)
	}
	if nat.UUID == "" {
		nat.UUID = utils.GenerateResourceID("nat")
	}
	stored := *nat
	m.NATs[nat.UUID] = &stored
	lr.NAT = append(lr.NAT, nat.UUID)
	return nil
}

func (m *Client) DeleteNAT(_ context.Context, routerName string, natType, logicalIP string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	lr, exists := m.Routers[routerName]
	if !exists {
		return fmt.Errorf("logical router %q not found", routerName)
	}
	// Find the NAT entry
	var foundUUID string
	for uuid, n := range m.NATs {
		if n.Type == natType && n.LogicalIP == logicalIP {
			foundUUID = uuid
			break
		}
	}
	if foundUUID == "" {
		return fmt.Errorf("NAT %s %s: %w", natType, logicalIP, ovn.ErrNATNotFound)
	}
	// Remove from router's NAT list
	for i, uuid := range lr.NAT {
		if uuid == foundUUID {
			lr.NAT = append(lr.NAT[:i], lr.NAT[i+1:]...)
			break
		}
	}
	delete(m.NATs, foundUUID)
	return nil
}

func (m *Client) DeleteNATByExternalIP(_ context.Context, routerName string, natType, externalIP string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	lr, exists := m.Routers[routerName]
	if !exists {
		return fmt.Errorf("logical router %q not found", routerName)
	}
	var foundUUIDs []string
	for uuid, n := range m.NATs {
		if n.Type == natType && n.ExternalIP == externalIP {
			foundUUIDs = append(foundUUIDs, uuid)
		}
	}
	if len(foundUUIDs) == 0 {
		return fmt.Errorf("NAT %s external_ip=%s: %w", natType, externalIP, ovn.ErrNATNotFound)
	}
	for _, foundUUID := range foundUUIDs {
		for i, uuid := range lr.NAT {
			if uuid == foundUUID {
				lr.NAT = append(lr.NAT[:i], lr.NAT[i+1:]...)
				break
			}
		}
		delete(m.NATs, foundUUID)
	}
	return nil
}

func (m *Client) DeleteAllNATsByExternalIP(_ context.Context, natType, externalIP string) (int, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()

	// Find all matching NAT rules.
	var foundUUIDs []string
	for uuid, n := range m.NATs {
		if n.Type == natType && n.ExternalIP == externalIP {
			foundUUIDs = append(foundUUIDs, uuid)
		}
	}
	if len(foundUUIDs) == 0 {
		return 0, nil
	}

	// Remove from all routers that reference them.
	uuidSet := make(map[string]struct{}, len(foundUUIDs))
	for _, u := range foundUUIDs {
		uuidSet[u] = struct{}{}
	}
	for _, lr := range m.Routers {
		filtered := lr.NAT[:0]
		for _, uuid := range lr.NAT {
			if _, stale := uuidSet[uuid]; !stale {
				filtered = append(filtered, uuid)
			}
		}
		lr.NAT = filtered
	}

	// Delete the NAT rows.
	for _, u := range foundUUIDs {
		delete(m.NATs, u)
	}
	return len(foundUUIDs), nil
}

func (m *Client) FindNATByExternalIP(_ context.Context, natType, externalIP string) (*nbdb.NAT, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	for _, n := range m.NATs {
		if n.Type == natType && n.ExternalIP == externalIP {
			return n, nil
		}
	}
	return nil, nil
}

// Static Routes

func (m *Client) AddStaticRoute(_ context.Context, routerName string, route *nbdb.LogicalRouterStaticRoute) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	lr, exists := m.Routers[routerName]
	if !exists {
		return fmt.Errorf("logical router %q not found", routerName)
	}
	if route.UUID == "" {
		route.UUID = utils.GenerateResourceID("route")
	}
	stored := *route
	m.StaticRoutes[route.UUID] = &stored
	lr.StaticRoutes = append(lr.StaticRoutes, route.UUID)
	return nil
}

func (m *Client) FindStaticRoute(_ context.Context, routerName, ipPrefix string) (*nbdb.LogicalRouterStaticRoute, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	lr, exists := m.Routers[routerName]
	if !exists {
		return nil, fmt.Errorf("logical router %q not found", routerName)
	}
	for _, uuid := range lr.StaticRoutes {
		r, ok := m.StaticRoutes[uuid]
		if !ok {
			continue
		}
		if r.IPPrefix == ipPrefix {
			result := *r
			return &result, nil
		}
	}
	return nil, nil
}

func (m *Client) DeleteStaticRoute(_ context.Context, routerName string, ipPrefix string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	lr, exists := m.Routers[routerName]
	if !exists {
		return fmt.Errorf("logical router %q not found", routerName)
	}
	// Find the route
	var foundUUID string
	for uuid, r := range m.StaticRoutes {
		if r.IPPrefix == ipPrefix {
			foundUUID = uuid
			break
		}
	}
	if foundUUID == "" {
		return fmt.Errorf("static route %s not found", ipPrefix)
	}
	// Remove from router's StaticRoutes list
	for i, uuid := range lr.StaticRoutes {
		if uuid == foundUUID {
			lr.StaticRoutes = append(lr.StaticRoutes[:i], lr.StaticRoutes[i+1:]...)
			break
		}
	}
	delete(m.StaticRoutes, foundUUID)
	return nil
}

// Port Groups

func (m *Client) CreatePortGroup(_ context.Context, name string, ports []string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if _, exists := m.PortGroups[name]; exists {
		return fmt.Errorf("port group %q already exists", name)
	}
	pg := &nbdb.PortGroup{
		UUID:  utils.GenerateResourceID("pg"),
		Name:  name,
		Ports: ports,
	}
	m.PortGroups[name] = pg
	return nil
}

// EnsurePortGroup mirrors the live client's wait-op semantics under the
// mock's single mutex.
func (m *Client) EnsurePortGroup(_ context.Context, name string, ports []string) (*nbdb.PortGroup, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if existing, ok := m.PortGroups[name]; ok {
		result := *existing
		return &result, nil
	}
	pg := &nbdb.PortGroup{
		UUID:  utils.GenerateResourceID("pg"),
		Name:  name,
		Ports: ports,
	}
	m.PortGroups[name] = pg
	result := *pg
	return &result, nil
}

func (m *Client) DeletePortGroup(_ context.Context, name string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	pg, exists := m.PortGroups[name]
	if !exists {
		return fmt.Errorf("port group %q not found", name)
	}
	// Mirror libovsdb's reference-integrity rejection: a port group with
	// referenced ACL rows cannot be deleted directly. Live OVN's
	// `Where(pg).Delete()` only removes the PG row and would leak the ACLs;
	// forcing every caller through ClearACLs first matches that contract.
	if len(pg.ACLs) > 0 {
		return fmt.Errorf("port group %q has %d ACLs still attached; clear ACLs before delete", name, len(pg.ACLs))
	}
	delete(m.PortGroups, name)
	return nil
}

// UpdatePortGroupMemberships applies all add/remove port-group joins for one
// LSP. SB Address_Set rows for `<pg>_ip4` / `<pg>_ip6` are auto-derived by
// ovn-northd in production and intentionally not modelled in the mock.
func (m *Client) UpdatePortGroupMemberships(_ context.Context, lspName string, addPGs, removePGs []string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	lsp, exists := m.Ports[lspName]
	if !exists {
		return fmt.Errorf("logical switch port %q not found", lspName)
	}
	for _, pgName := range addPGs {
		pg, exists := m.PortGroups[pgName]
		if !exists {
			return fmt.Errorf("port group %q not found", pgName)
		}
		if !slices.Contains(pg.Ports, lsp.UUID) {
			pg.Ports = append(pg.Ports, lsp.UUID)
		}
	}
	for _, pgName := range removePGs {
		pg, exists := m.PortGroups[pgName]
		if !exists {
			return fmt.Errorf("port group %q not found", pgName)
		}
		for i, u := range pg.Ports {
			if u == lsp.UUID {
				pg.Ports = append(pg.Ports[:i], pg.Ports[i+1:]...)
				break
			}
		}
	}
	return nil
}

// ListPortGroupsForPort returns names of port groups whose Ports contains the
// given LSP's UUID. Mirrors the live client; the mock returns an empty slice
// (not an error) when the LSP exists but has no memberships, and an error
// only when the LSP itself is unknown.
func (m *Client) ListPortGroupsForPort(_ context.Context, lspName string) ([]string, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	lsp, exists := m.Ports[lspName]
	if !exists {
		return nil, fmt.Errorf("logical switch port %q not found", lspName)
	}
	names := make([]string, 0)
	for name, pg := range m.PortGroups {
		if slices.Contains(pg.Ports, lsp.UUID) {
			names = append(names, name)
		}
	}
	return names, nil
}

// GetPortGroup returns the port group with the given name, or an error if it
// doesn't exist. Mirrors the live client; missing rows surface as
// ovn.ErrPortGroupNotFound so callers can use errors.Is for idempotent flows.
func (m *Client) GetPortGroup(_ context.Context, name string) (*nbdb.PortGroup, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	pg, exists := m.PortGroups[name]
	if !exists {
		return nil, fmt.Errorf("%w: %q", ovn.ErrPortGroupNotFound, name)
	}
	return pg, nil
}

// ListPortGroups returns a snapshot of every port group currently in the mock
// store. The slice is freshly allocated; mutating it does not affect the mock.
func (m *Client) ListPortGroups(_ context.Context) ([]nbdb.PortGroup, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	out := make([]nbdb.PortGroup, 0, len(m.PortGroups))
	for _, pg := range m.PortGroups {
		out = append(out, *pg)
	}
	return out, nil
}

// ACLs

func (m *Client) AddACLs(_ context.Context, portGroupName string, specs []ovn.ACLSpec) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.AddACLCalls++
	if m.AddACLErrAfter > 0 && m.AddACLCalls == m.AddACLErrAfter {
		return fmt.Errorf("injected AddACLs failure on call %d", m.AddACLCalls)
	}
	pg, exists := m.PortGroups[portGroupName]
	if !exists {
		return fmt.Errorf("port group %q not found", portGroupName)
	}
	for _, spec := range specs {
		acl := &nbdb.ACL{
			UUID:      utils.GenerateResourceID("acl"),
			Direction: spec.Direction,
			Priority:  spec.Priority,
			Match:     spec.Match,
			Action:    spec.Action,
			Log:       spec.Log,
		}
		if spec.Name != "" {
			name := spec.Name
			acl.Name = &name
		}
		if spec.Severity != "" {
			severity := spec.Severity
			acl.Severity = &severity
		}
		m.ACLs[acl.UUID] = acl
		pg.ACLs = append(pg.ACLs, acl.UUID)
	}
	return nil
}

func (m *Client) ClearACLs(_ context.Context, portGroupName string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	pg, exists := m.PortGroups[portGroupName]
	if !exists {
		return fmt.Errorf("port group %q not found", portGroupName)
	}
	for _, aclUUID := range pg.ACLs {
		delete(m.ACLs, aclUUID)
	}
	pg.ACLs = nil
	return nil
}

// ListLogicalRouterPorts returns every LRP across the mock state.
func (m *Client) ListLogicalRouterPorts(_ context.Context) ([]nbdb.LogicalRouterPort, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	result := make([]nbdb.LogicalRouterPort, 0, len(m.RouterPorts))
	for _, lrp := range m.RouterPorts {
		result = append(result, *lrp)
	}
	return result, nil
}

// SetGatewayChassis is the idempotent read-then-decide path mirrored from
// LiveClient. Tests rely on the mock applying the same "no-op when already
// correct" semantics so the call counters distinguish create vs. update vs.
// no-op (mulga-999).
func (m *Client) SetGatewayChassis(_ context.Context, lrpName string, chassisName string, priority int) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	lrp, exists := m.RouterPorts[lrpName]
	if !exists {
		return fmt.Errorf("logical router port %q not found", lrpName)
	}
	gcName := lrpName + "-" + chassisName
	for _, gc := range m.GatewayChassis {
		if gc.Name != gcName {
			continue
		}
		if gc.Priority == priority {
			return nil
		}
		gc.Priority = priority
		m.UpdateGatewayChassisPriorityCalls++
		return nil
	}
	gc := &nbdb.GatewayChassis{
		UUID:        utils.GenerateResourceID("gc"),
		Name:        gcName,
		ChassisName: chassisName,
		Priority:    priority,
		ExternalIDs: map[string]string{},
		Options:     map[string]string{},
	}
	m.GatewayChassis[gc.UUID] = gc
	lrp.GatewayChassis = append(lrp.GatewayChassis, gc.UUID)
	m.SetGatewayChassisCalls++
	return nil
}

func (m *Client) GetGatewayChassisByName(_ context.Context, name string) (*nbdb.GatewayChassis, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	for _, gc := range m.GatewayChassis {
		if gc.Name == name {
			result := *gc
			return &result, nil
		}
	}
	return nil, nil
}

func (m *Client) ListGatewayChassis(_ context.Context) ([]nbdb.GatewayChassis, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	result := make([]nbdb.GatewayChassis, 0, len(m.GatewayChassis))
	for _, gc := range m.GatewayChassis {
		result = append(result, *gc)
	}
	return result, nil
}

func (m *Client) DeleteGatewayChassis(_ context.Context, lrpName string, gcUUID string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	lrp, exists := m.RouterPorts[lrpName]
	if !exists {
		return fmt.Errorf("logical router port %q not found", lrpName)
	}
	if _, ok := m.GatewayChassis[gcUUID]; !ok {
		return fmt.Errorf("gateway_chassis %q not found", gcUUID)
	}
	for i, u := range lrp.GatewayChassis {
		if u == gcUUID {
			lrp.GatewayChassis = append(lrp.GatewayChassis[:i], lrp.GatewayChassis[i+1:]...)
			break
		}
	}
	delete(m.GatewayChassis, gcUUID)
	m.DeleteGatewayChassisCalls++
	return nil
}

// SeedGatewayChassis lets tests pre-populate a Gateway_Chassis row directly,
// bypassing the idempotent SetGatewayChassis path. Useful for setting up a
// "stale row" scenario for reconcileGatewayChassis tests (mulga-999).
func (m *Client) SeedGatewayChassis(lrpName string, gc *nbdb.GatewayChassis) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if gc.UUID == "" {
		gc.UUID = utils.GenerateResourceID("gc")
	}
	m.GatewayChassis[gc.UUID] = gc
	if lrp, ok := m.RouterPorts[lrpName]; ok {
		lrp.GatewayChassis = append(lrp.GatewayChassis, gc.UUID)
	}
}
