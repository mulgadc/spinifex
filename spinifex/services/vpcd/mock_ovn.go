package vpcd

import (
	"context"
	"fmt"
	"sync"

	"github.com/mulgadc/spinifex/spinifex/services/vpcd/nbdb"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// MockOVNClient implements OVNClient with in-memory storage for testing.
type MockOVNClient struct {
	mu        sync.Mutex
	connected bool

	switches     map[string]*nbdb.LogicalSwitch
	ports        map[string]*nbdb.LogicalSwitchPort
	routers      map[string]*nbdb.LogicalRouter
	routerPorts  map[string]*nbdb.LogicalRouterPort
	dhcpOpts     map[string]*nbdb.DHCPOptions
	nats         map[string]*nbdb.NAT                      // keyed by UUID
	staticRoutes map[string]*nbdb.LogicalRouterStaticRoute // keyed by UUID
	portGroups   map[string]*nbdb.PortGroup                // keyed by name
	acls         map[string]*nbdb.ACL                      // keyed by UUID

	// UpdateLogicalSwitchPortCalls counts UpdateLogicalSwitchPort invocations
	// so tests can assert idempotent read-before-write paths (e.g.
	// ensureLocalnetOptions — mulga-998.b Fix 3).
	UpdateLogicalSwitchPortCalls int
}

// NewMockOVNClient creates a new MockOVNClient for testing.
func NewMockOVNClient() *MockOVNClient {
	return &MockOVNClient{
		switches:     make(map[string]*nbdb.LogicalSwitch),
		ports:        make(map[string]*nbdb.LogicalSwitchPort),
		routers:      make(map[string]*nbdb.LogicalRouter),
		routerPorts:  make(map[string]*nbdb.LogicalRouterPort),
		dhcpOpts:     make(map[string]*nbdb.DHCPOptions),
		nats:         make(map[string]*nbdb.NAT),
		staticRoutes: make(map[string]*nbdb.LogicalRouterStaticRoute),
		portGroups:   make(map[string]*nbdb.PortGroup),
		acls:         make(map[string]*nbdb.ACL),
	}
}

func (m *MockOVNClient) Connect(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected = true
	return nil
}

func (m *MockOVNClient) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected = false
}

func (m *MockOVNClient) Connected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected
}

// Logical Switch

func (m *MockOVNClient) CreateLogicalSwitch(_ context.Context, ls *nbdb.LogicalSwitch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.switches[ls.Name]; exists {
		return fmt.Errorf("logical switch %q already exists", ls.Name)
	}
	if ls.UUID == "" {
		ls.UUID = utils.GenerateResourceID("ovn")
	}
	stored := *ls
	m.switches[ls.Name] = &stored
	return nil
}

func (m *MockOVNClient) DeleteLogicalSwitch(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.switches[name]; !exists {
		return fmt.Errorf("logical switch %q not found", name)
	}
	delete(m.switches, name)
	return nil
}

func (m *MockOVNClient) GetLogicalSwitch(_ context.Context, name string) (*nbdb.LogicalSwitch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ls, exists := m.switches[name]
	if !exists {
		return nil, fmt.Errorf("logical switch %q not found", name)
	}
	result := *ls
	return &result, nil
}

func (m *MockOVNClient) ListLogicalSwitches(_ context.Context) ([]nbdb.LogicalSwitch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]nbdb.LogicalSwitch, 0, len(m.switches))
	for _, ls := range m.switches {
		result = append(result, *ls)
	}
	return result, nil
}

// Logical Switch Port

func (m *MockOVNClient) CreateLogicalSwitchPort(_ context.Context, switchName string, lsp *nbdb.LogicalSwitchPort) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ls, exists := m.switches[switchName]
	if !exists {
		return fmt.Errorf("logical switch %q not found", switchName)
	}
	if _, exists := m.ports[lsp.Name]; exists {
		return fmt.Errorf("logical switch port %q already exists", lsp.Name)
	}
	if lsp.UUID == "" {
		lsp.UUID = utils.GenerateResourceID("ovn")
	}
	stored := *lsp
	m.ports[lsp.Name] = &stored
	ls.Ports = append(ls.Ports, lsp.UUID)
	return nil
}

func (m *MockOVNClient) DeleteLogicalSwitchPort(_ context.Context, switchName string, portName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	port, exists := m.ports[portName]
	if !exists {
		return fmt.Errorf("logical switch port %q not found", portName)
	}
	ls, exists := m.switches[switchName]
	if !exists {
		return fmt.Errorf("logical switch %q not found", switchName)
	}
	// Remove port UUID from switch's ports list
	for i, uuid := range ls.Ports {
		if uuid == port.UUID {
			ls.Ports = append(ls.Ports[:i], ls.Ports[i+1:]...)
			break
		}
	}
	delete(m.ports, portName)
	return nil
}

func (m *MockOVNClient) GetLogicalSwitchPort(_ context.Context, name string) (*nbdb.LogicalSwitchPort, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lsp, exists := m.ports[name]
	if !exists {
		return nil, fmt.Errorf("logical switch port %q not found", name)
	}
	result := *lsp
	return &result, nil
}

func (m *MockOVNClient) UpdateLogicalSwitchPort(_ context.Context, lsp *nbdb.LogicalSwitchPort) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.ports[lsp.Name]; !exists {
		return fmt.Errorf("logical switch port %q not found", lsp.Name)
	}
	stored := *lsp
	m.ports[lsp.Name] = &stored
	m.UpdateLogicalSwitchPortCalls++
	return nil
}

// Logical Router

func (m *MockOVNClient) CreateLogicalRouter(_ context.Context, lr *nbdb.LogicalRouter) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.routers[lr.Name]; exists {
		return fmt.Errorf("logical router %q already exists", lr.Name)
	}
	if lr.UUID == "" {
		lr.UUID = utils.GenerateResourceID("ovn")
	}
	stored := *lr
	m.routers[lr.Name] = &stored
	return nil
}

func (m *MockOVNClient) DeleteLogicalRouter(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.routers[name]; !exists {
		return fmt.Errorf("logical router %q not found", name)
	}
	delete(m.routers, name)
	return nil
}

func (m *MockOVNClient) GetLogicalRouter(_ context.Context, name string) (*nbdb.LogicalRouter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lr, exists := m.routers[name]
	if !exists {
		return nil, fmt.Errorf("logical router %q not found", name)
	}
	result := *lr
	return &result, nil
}

func (m *MockOVNClient) ListLogicalRouters(_ context.Context) ([]nbdb.LogicalRouter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]nbdb.LogicalRouter, 0, len(m.routers))
	for _, lr := range m.routers {
		result = append(result, *lr)
	}
	return result, nil
}

// Logical Router Port

func (m *MockOVNClient) CreateLogicalRouterPort(_ context.Context, routerName string, lrp *nbdb.LogicalRouterPort) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	lr, exists := m.routers[routerName]
	if !exists {
		return fmt.Errorf("logical router %q not found", routerName)
	}
	if _, exists := m.routerPorts[lrp.Name]; exists {
		return fmt.Errorf("logical router port %q already exists", lrp.Name)
	}
	if lrp.UUID == "" {
		lrp.UUID = utils.GenerateResourceID("ovn")
	}
	stored := *lrp
	m.routerPorts[lrp.Name] = &stored
	lr.Ports = append(lr.Ports, lrp.UUID)
	return nil
}

func (m *MockOVNClient) DeleteLogicalRouterPort(_ context.Context, routerName string, portName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	port, exists := m.routerPorts[portName]
	if !exists {
		return fmt.Errorf("logical router port %q not found", portName)
	}
	lr, exists := m.routers[routerName]
	if !exists {
		return fmt.Errorf("logical router %q not found", routerName)
	}
	for i, uuid := range lr.Ports {
		if uuid == port.UUID {
			lr.Ports = append(lr.Ports[:i], lr.Ports[i+1:]...)
			break
		}
	}
	delete(m.routerPorts, portName)
	return nil
}

func (m *MockOVNClient) GetLogicalRouterPort(_ context.Context, name string) (*nbdb.LogicalRouterPort, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lrp, exists := m.routerPorts[name]
	if !exists {
		return nil, fmt.Errorf("logical router port %q not found", name)
	}
	result := *lrp
	return &result, nil
}

// DHCP Options

func (m *MockOVNClient) CreateDHCPOptions(_ context.Context, opts *nbdb.DHCPOptions) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if opts.UUID == "" {
		opts.UUID = utils.GenerateResourceID("dhcp")
	}
	stored := *opts
	m.dhcpOpts[opts.UUID] = &stored
	return opts.UUID, nil
}

func (m *MockOVNClient) DeleteDHCPOptions(_ context.Context, uuid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.dhcpOpts[uuid]; !exists {
		return fmt.Errorf("DHCP options %q not found", uuid)
	}
	delete(m.dhcpOpts, uuid)
	return nil
}

func (m *MockOVNClient) FindDHCPOptionsByCIDR(_ context.Context, cidr string) (*nbdb.DHCPOptions, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, opts := range m.dhcpOpts {
		if opts.CIDR == cidr {
			result := *opts
			return &result, nil
		}
	}
	return nil, fmt.Errorf("DHCP options for CIDR %q not found", cidr)
}

func (m *MockOVNClient) FindDHCPOptionsByExternalID(_ context.Context, key, value string) (*nbdb.DHCPOptions, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, opts := range m.dhcpOpts {
		if opts.ExternalIDs[key] == value {
			result := *opts
			return &result, nil
		}
	}
	return nil, fmt.Errorf("DHCP options with external_id %s=%s not found", key, value)
}

func (m *MockOVNClient) ListDHCPOptions(_ context.Context) ([]nbdb.DHCPOptions, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]nbdb.DHCPOptions, 0, len(m.dhcpOpts))
	for _, opts := range m.dhcpOpts {
		result = append(result, *opts)
	}
	return result, nil
}

// NAT

func (m *MockOVNClient) AddNAT(_ context.Context, routerName string, nat *nbdb.NAT) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	lr, exists := m.routers[routerName]
	if !exists {
		return fmt.Errorf("logical router %q not found", routerName)
	}
	if nat.UUID == "" {
		nat.UUID = utils.GenerateResourceID("nat")
	}
	stored := *nat
	m.nats[nat.UUID] = &stored
	lr.NAT = append(lr.NAT, nat.UUID)
	return nil
}

func (m *MockOVNClient) DeleteNAT(_ context.Context, routerName string, natType, logicalIP string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	lr, exists := m.routers[routerName]
	if !exists {
		return fmt.Errorf("logical router %q not found", routerName)
	}
	// Find the NAT entry
	var foundUUID string
	for uuid, n := range m.nats {
		if n.Type == natType && n.LogicalIP == logicalIP {
			foundUUID = uuid
			break
		}
	}
	if foundUUID == "" {
		return fmt.Errorf("NAT %s %s not found", natType, logicalIP)
	}
	// Remove from router's NAT list
	for i, uuid := range lr.NAT {
		if uuid == foundUUID {
			lr.NAT = append(lr.NAT[:i], lr.NAT[i+1:]...)
			break
		}
	}
	delete(m.nats, foundUUID)
	return nil
}

func (m *MockOVNClient) DeleteNATByExternalIP(_ context.Context, routerName string, natType, externalIP string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	lr, exists := m.routers[routerName]
	if !exists {
		return fmt.Errorf("logical router %q not found", routerName)
	}
	var foundUUIDs []string
	for uuid, n := range m.nats {
		if n.Type == natType && n.ExternalIP == externalIP {
			foundUUIDs = append(foundUUIDs, uuid)
		}
	}
	if len(foundUUIDs) == 0 {
		return fmt.Errorf("NAT %s external_ip=%s not found", natType, externalIP)
	}
	for _, foundUUID := range foundUUIDs {
		for i, uuid := range lr.NAT {
			if uuid == foundUUID {
				lr.NAT = append(lr.NAT[:i], lr.NAT[i+1:]...)
				break
			}
		}
		delete(m.nats, foundUUID)
	}
	return nil
}

func (m *MockOVNClient) DeleteAllNATsByExternalIP(_ context.Context, natType, externalIP string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Find all matching NAT rules.
	var foundUUIDs []string
	for uuid, n := range m.nats {
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
	for _, lr := range m.routers {
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
		delete(m.nats, u)
	}
	return len(foundUUIDs), nil
}

// Static Routes

func (m *MockOVNClient) AddStaticRoute(_ context.Context, routerName string, route *nbdb.LogicalRouterStaticRoute) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	lr, exists := m.routers[routerName]
	if !exists {
		return fmt.Errorf("logical router %q not found", routerName)
	}
	if route.UUID == "" {
		route.UUID = utils.GenerateResourceID("route")
	}
	stored := *route
	m.staticRoutes[route.UUID] = &stored
	lr.StaticRoutes = append(lr.StaticRoutes, route.UUID)
	return nil
}

func (m *MockOVNClient) DeleteStaticRoute(_ context.Context, routerName string, ipPrefix string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	lr, exists := m.routers[routerName]
	if !exists {
		return fmt.Errorf("logical router %q not found", routerName)
	}
	// Find the route
	var foundUUID string
	for uuid, r := range m.staticRoutes {
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
	delete(m.staticRoutes, foundUUID)
	return nil
}

// Port Groups

func (m *MockOVNClient) CreatePortGroup(_ context.Context, name string, ports []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.portGroups[name]; exists {
		return fmt.Errorf("port group %q already exists", name)
	}
	pg := &nbdb.PortGroup{
		UUID:  utils.GenerateResourceID("pg"),
		Name:  name,
		Ports: ports,
	}
	m.portGroups[name] = pg
	return nil
}

func (m *MockOVNClient) DeletePortGroup(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.portGroups[name]; !exists {
		return fmt.Errorf("port group %q not found", name)
	}
	// Remove ACLs associated with this port group
	pg := m.portGroups[name]
	for _, aclUUID := range pg.ACLs {
		delete(m.acls, aclUUID)
	}
	delete(m.portGroups, name)
	return nil
}

func (m *MockOVNClient) SetPortGroupPorts(_ context.Context, name string, ports []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pg, exists := m.portGroups[name]
	if !exists {
		return fmt.Errorf("port group %q not found", name)
	}
	pg.Ports = ports
	return nil
}

// ACLs

func (m *MockOVNClient) AddACL(_ context.Context, portGroupName string, spec ACLSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pg, exists := m.portGroups[portGroupName]
	if !exists {
		return fmt.Errorf("port group %q not found", portGroupName)
	}
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
	m.acls[acl.UUID] = acl
	pg.ACLs = append(pg.ACLs, acl.UUID)
	return nil
}

func (m *MockOVNClient) ClearACLs(_ context.Context, portGroupName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pg, exists := m.portGroups[portGroupName]
	if !exists {
		return fmt.Errorf("port group %q not found", portGroupName)
	}
	for _, aclUUID := range pg.ACLs {
		delete(m.acls, aclUUID)
	}
	pg.ACLs = nil
	return nil
}

func (m *MockOVNClient) SetGatewayChassis(_ context.Context, lrpName string, chassisName string, priority int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	lrp, exists := m.routerPorts[lrpName]
	if !exists {
		return fmt.Errorf("logical router port %q not found", lrpName)
	}
	gcName := lrpName + "-" + chassisName
	lrp.GatewayChassis = append(lrp.GatewayChassis, gcName)
	_ = priority
	return nil
}
