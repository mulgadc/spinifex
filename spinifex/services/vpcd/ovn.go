package vpcd

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/services/vpcd/nbdb"
	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

// transactOps executes a set of OVSDB operations as a single transaction,
// checking both the RPC error and individual operation results.
func (c *LiveOVNClient) transactOps(ctx context.Context, ops []ovsdb.Operation) error {
	results, err := c.client.Transact(ctx, ops...)
	if err != nil {
		return err
	}
	_, err = ovsdb.CheckOperationResults(results, ops)
	if err != nil {
		// Log detailed per-operation errors for debugging
		for i, r := range results {
			if r.Error != "" {
				opTable := ""
				if i < len(ops) {
					opTable = fmt.Sprintf("%s on %s", ops[i].Op, ops[i].Table)
				}
				slog.Error("OVSDB operation failed", "index", i, "op", opTable, "error", r.Error, "details", r.Details)
			}
		}
	}
	return err
}

// ACLSpec describes an OVN ACL rule for attachment to a port group.
// Name, Severity, and Meter are optional — set Severity when Log is true.
type ACLSpec struct {
	Direction string // "to-lport" or "from-lport"
	Priority  int
	Match     string
	Action    string // "allow-related", "drop", "allow", "reject"
	Name      string
	Log       bool
	Severity  string // "alert", "warning", "notice", "info", "debug"
}

// OVNClient defines the interface for interacting with the OVN Northbound Database.
// This abstraction allows for mock implementations in tests.
type OVNClient interface {
	// Connection lifecycle
	Connect(ctx context.Context) error
	Close()
	Connected() bool

	// Logical Switch (subnet)
	CreateLogicalSwitch(ctx context.Context, ls *nbdb.LogicalSwitch) error
	DeleteLogicalSwitch(ctx context.Context, name string) error
	GetLogicalSwitch(ctx context.Context, name string) (*nbdb.LogicalSwitch, error)
	ListLogicalSwitches(ctx context.Context) ([]nbdb.LogicalSwitch, error)

	// Logical Switch Port (VM/ENI)
	CreateLogicalSwitchPort(ctx context.Context, switchName string, lsp *nbdb.LogicalSwitchPort) error
	// CreateLogicalSwitchPortInGroups creates an LSP, adds it to its switch,
	// joins it to the named port groups, and inserts privateIP into each
	// port group's address set — all in a single OVSDB transaction. Required
	// for SG enforcement: a non-atomic create-then-join leaves a window where
	// the LSP exists outside any port group (OVN default = unrestricted) or
	// where ports are members but their IPs aren't in the SG-to-SG address
	// set (peer SGs treat the port as nonexistent and reject its traffic).
	// privateIP may be "" and portGroupNames may be empty (e.g. router/
	// localnet ports), in which case the address-set inserts are skipped.
	CreateLogicalSwitchPortInGroups(ctx context.Context, switchName string, lsp *nbdb.LogicalSwitchPort, portGroupNames []string, privateIP string) error
	DeleteLogicalSwitchPort(ctx context.Context, switchName string, portName string) error
	GetLogicalSwitchPort(ctx context.Context, name string) (*nbdb.LogicalSwitchPort, error)
	UpdateLogicalSwitchPort(ctx context.Context, lsp *nbdb.LogicalSwitchPort) error

	// Logical Router (VPC router)
	CreateLogicalRouter(ctx context.Context, lr *nbdb.LogicalRouter) error
	DeleteLogicalRouter(ctx context.Context, name string) error
	GetLogicalRouter(ctx context.Context, name string) (*nbdb.LogicalRouter, error)
	ListLogicalRouters(ctx context.Context) ([]nbdb.LogicalRouter, error)

	// Logical Router Port
	CreateLogicalRouterPort(ctx context.Context, routerName string, lrp *nbdb.LogicalRouterPort) error
	DeleteLogicalRouterPort(ctx context.Context, routerName string, portName string) error
	GetLogicalRouterPort(ctx context.Context, name string) (*nbdb.LogicalRouterPort, error)
	UpdateLogicalRouterPort(ctx context.Context, lrp *nbdb.LogicalRouterPort) error
	ListLogicalRouterPorts(ctx context.Context) ([]nbdb.LogicalRouterPort, error)

	// DHCP Options
	CreateDHCPOptions(ctx context.Context, opts *nbdb.DHCPOptions) (string, error)
	DeleteDHCPOptions(ctx context.Context, uuid string) error
	FindDHCPOptionsByCIDR(ctx context.Context, cidr string) (*nbdb.DHCPOptions, error)
	FindDHCPOptionsByExternalID(ctx context.Context, key, value string) (*nbdb.DHCPOptions, error)
	ListDHCPOptions(ctx context.Context) ([]nbdb.DHCPOptions, error)

	// NAT rules
	AddNAT(ctx context.Context, routerName string, nat *nbdb.NAT) error
	DeleteNAT(ctx context.Context, routerName string, natType, logicalIP string) error
	DeleteNATByExternalIP(ctx context.Context, routerName string, natType, externalIP string) error
	DeleteAllNATsByExternalIP(ctx context.Context, natType, externalIP string) (int, error)
	FindNATByExternalIP(ctx context.Context, natType, externalIP string) (*nbdb.NAT, error)

	// Static routes
	AddStaticRoute(ctx context.Context, routerName string, route *nbdb.LogicalRouterStaticRoute) error
	DeleteStaticRoute(ctx context.Context, routerName string, ipPrefix string) error

	// Port Groups (security group enforcement)
	CreatePortGroup(ctx context.Context, name string, ports []string) error
	DeletePortGroup(ctx context.Context, name string) error
	// UpdatePortGroupMemberships applies all port-group joins, port-group
	// leaves, and the matching address-set entry inserts/removes for a single
	// LSP in one atomic OVSDB transaction. Required by reconcilePortSGs so a
	// 5-SG → different-5-SG modify never exposes an intermediate state with
	// fewer port groups (which would let the OVN default = unrestricted apply
	// for the gap). privateIP may be "" to skip address-set updates.
	UpdatePortGroupMemberships(ctx context.Context, lspName, privateIP string, addPGs, removePGs []string) error
	// ListPortGroupsForPort returns the names of every port group whose Ports
	// set contains the given LSP. Used by reconcilePortSGs to discover current
	// membership before computing the add/remove diff against desired.
	ListPortGroupsForPort(ctx context.Context, lspName string) ([]string, error)
	// GetPortGroup returns the port group with the given name, or an error if
	// it doesn't exist. Used by the reconciler to detect SGs whose port group
	// has gone missing in OVN NB.
	GetPortGroup(ctx context.Context, name string) (*nbdb.PortGroup, error)
	// ListPortGroups returns every port group in OVN NB. Used by the
	// reconciler's orphan-PG scan to detect spinifex-managed port groups
	// (`sg_*`) that no longer have a matching SG record in KV.
	ListPortGroups(ctx context.Context) ([]nbdb.PortGroup, error)

	// ACLs (attached to port groups). AddACLs creates all rows and links
	// them to the port group in one OVSDB transaction — important when a
	// single SG can carry up to 60 ingress + 60 egress rules.
	AddACLs(ctx context.Context, portGroupName string, specs []ACLSpec) error
	ClearACLs(ctx context.Context, portGroupName string) error

	// Address Sets (referenced by ACL match expressions for SG-to-SG rules)
	CreateAddressSet(ctx context.Context, name string, addresses []string) error
	DeleteAddressSet(ctx context.Context, name string) error
	AddAddressSetEntry(ctx context.Context, name string, address string) error
	RemoveAddressSetEntry(ctx context.Context, name string, address string) error

	// Gateway Chassis (HA scheduling for gateway router ports)
	SetGatewayChassis(ctx context.Context, lrpName string, chassisName string, priority int) error
	GetGatewayChassisByName(ctx context.Context, name string) (*nbdb.GatewayChassis, error)
	ListGatewayChassis(ctx context.Context) ([]nbdb.GatewayChassis, error)
	DeleteGatewayChassis(ctx context.Context, lrpName string, gcUUID string) error
}

// namedUUID generates a valid OVSDB named-uuid from a prefix and name.
// OVSDB named-uuids must match [_a-zA-Z][_a-zA-Z0-9]* — anything outside
// that class (spaces, '@', '=', '&', '-', '.', '/', etc.) is replaced with
// an underscore. Callers always supply a prefix that begins with a letter,
// so the leading-character constraint is satisfied implicitly.
//
// Without exhaustive sanitisation, ACL named-uuids that embed a Match
// expression like "outport == @pg-sg-XYZ && ip4" produce strings with
// spaces and '@' that OVSDB rejects with "Type mismatch for member
// 'uuid-name'", which silently breaks every default-SG ACL transaction.
func namedUUID(prefix, name string) string {
	s := prefix + name
	result := make([]byte, len(s))
	for i := range s {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_':
			result[i] = c
		default:
			result[i] = '_'
		}
	}
	return string(result)
}

// LiveOVNClient implements OVNClient using libovsdb against a real OVN NB DB.
type LiveOVNClient struct {
	endpoint string
	client   client.Client
}

var _ OVNClient = (*LiveOVNClient)(nil)

// NewLiveOVNClient creates a new LiveOVNClient targeting the given OVN NB DB endpoint.
// The endpoint should be in the format "tcp:host:port" or "unix:/path/to/socket".
func NewLiveOVNClient(endpoint string) *LiveOVNClient {
	return &LiveOVNClient{endpoint: endpoint}
}

func (c *LiveOVNClient) Connect(ctx context.Context) error {
	dbModel, err := nbdb.FullDatabaseModel()
	if err != nil {
		return fmt.Errorf("failed to create database model: %w", err)
	}

	ovn, err := client.NewOVSDBClient(dbModel, client.WithEndpoint(c.endpoint))
	if err != nil {
		return fmt.Errorf("failed to create OVSDB client: %w", err)
	}

	if err := ovn.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect to OVN NB DB at %s: %w", c.endpoint, err)
	}

	_, err = ovn.MonitorAll(ctx)
	if err != nil {
		ovn.Close()
		return fmt.Errorf("failed to monitor OVN NB DB: %w", err)
	}

	c.client = ovn
	slog.Info("Connected to OVN NB DB", "endpoint", c.endpoint)
	return nil
}

func (c *LiveOVNClient) Close() {
	if c.client != nil {
		c.client.Close()
		slog.Info("Disconnected from OVN NB DB")
	}
}

func (c *LiveOVNClient) Connected() bool {
	return c.client != nil
}

func (c *LiveOVNClient) CreateLogicalSwitch(ctx context.Context, ls *nbdb.LogicalSwitch) error {
	ops, err := c.client.Create(ls)
	if err != nil {
		return fmt.Errorf("create logical switch ops: %w", err)
	}
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("create logical switch transact: %w", err)
	}
	return nil
}

func (c *LiveOVNClient) DeleteLogicalSwitch(ctx context.Context, name string) error {
	ls, err := c.GetLogicalSwitch(ctx, name)
	if err != nil {
		return fmt.Errorf("delete logical switch lookup: %w", err)
	}
	ops, err := c.client.Where(ls).Delete()
	if err != nil {
		return fmt.Errorf("delete logical switch ops: %w", err)
	}
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("delete logical switch transact: %w", err)
	}
	return nil
}

func (c *LiveOVNClient) GetLogicalSwitch(ctx context.Context, name string) (*nbdb.LogicalSwitch, error) {
	var switches []nbdb.LogicalSwitch
	err := c.client.WhereCache(func(ls *nbdb.LogicalSwitch) bool {
		return ls.Name == name
	}).List(ctx, &switches)
	if err != nil {
		return nil, fmt.Errorf("get logical switch: %w", err)
	}
	if len(switches) == 0 {
		return nil, fmt.Errorf("logical switch %q not found", name)
	}
	return &switches[0], nil
}

func (c *LiveOVNClient) ListLogicalSwitches(ctx context.Context) ([]nbdb.LogicalSwitch, error) {
	var switches []nbdb.LogicalSwitch
	err := c.client.List(ctx, &switches)
	if err != nil {
		return nil, fmt.Errorf("list logical switches: %w", err)
	}
	return switches, nil
}

func (c *LiveOVNClient) CreateLogicalSwitchPort(ctx context.Context, switchName string, lsp *nbdb.LogicalSwitchPort) error {
	// Set a named UUID so the port can be referenced in the same transaction
	if lsp.UUID == "" {
		lsp.UUID = namedUUID("lsp_", lsp.Name)
	}

	// Create the port
	createOps, err := c.client.Create(lsp)
	if err != nil {
		return fmt.Errorf("create logical switch port ops: %w", err)
	}

	// Look up the switch to get its UUID for the Where clause
	ls, err := c.GetLogicalSwitch(ctx, switchName)
	if err != nil {
		return fmt.Errorf("get logical switch for port add: %w", err)
	}

	// Add the port to the switch's ports set (uses named UUID from Create)
	mutateOps, err := c.client.Where(ls).Mutate(ls, model.Mutation{
		Field:   &ls.Ports,
		Mutator: "insert",
		Value:   []string{lsp.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate logical switch ports ops: %w", err)
	}

	ops := append(createOps, mutateOps...)
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("create logical switch port transact: %w", err)
	}
	return nil
}

// CreateLogicalSwitchPortInGroups bundles LSP create + switch ports mutate +
// per-port-group ports mutates + per-address-set entry mutates into one
// transaction. Within the transaction the LSP's named UUID resolves
// consistently across all ops, so the port group mutates can reference the
// not-yet-committed LSP without a cache lookup. privateIP is added to each
// port group's address set so SG-to-SG match expressions resolve from the
// moment the LSP becomes live.
func (c *LiveOVNClient) CreateLogicalSwitchPortInGroups(ctx context.Context, switchName string, lsp *nbdb.LogicalSwitchPort, portGroupNames []string, privateIP string) error {
	if lsp.UUID == "" {
		lsp.UUID = namedUUID("lsp_", lsp.Name)
	}

	createOps, err := c.client.Create(lsp)
	if err != nil {
		return fmt.Errorf("create logical switch port ops: %w", err)
	}

	ls, err := c.GetLogicalSwitch(ctx, switchName)
	if err != nil {
		return fmt.Errorf("get logical switch for port add: %w", err)
	}

	switchMutateOps, err := c.client.Where(ls).Mutate(ls, model.Mutation{
		Field:   &ls.Ports,
		Mutator: "insert",
		Value:   []string{lsp.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate logical switch ports ops: %w", err)
	}

	ops := append(createOps, switchMutateOps...)

	for _, pgName := range portGroupNames {
		pg, err := c.getPortGroup(ctx, pgName)
		if err != nil {
			return fmt.Errorf("get port group %s for port add: %w", pgName, err)
		}
		pgMutateOps, err := c.client.Where(pg).Mutate(pg, model.Mutation{
			Field:   &pg.Ports,
			Mutator: ovsdb.MutateOperationInsert,
			Value:   []string{lsp.UUID},
		})
		if err != nil {
			return fmt.Errorf("mutate port group %s ops: %w", pgName, err)
		}
		ops = append(ops, pgMutateOps...)

		if privateIP == "" {
			continue
		}
		asName := addressSetName(pgName)
		as, err := c.getAddressSet(ctx, asName)
		if err != nil {
			return fmt.Errorf("get address set %s for port add: %w", asName, err)
		}
		asMutateOps, err := c.client.Where(as).Mutate(as, model.Mutation{
			Field:   &as.Addresses,
			Mutator: ovsdb.MutateOperationInsert,
			Value:   []string{privateIP},
		})
		if err != nil {
			return fmt.Errorf("mutate address set %s ops: %w", asName, err)
		}
		ops = append(ops, asMutateOps...)
	}

	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("create logical switch port in groups transact: %w", err)
	}
	return nil
}

func (c *LiveOVNClient) DeleteLogicalSwitchPort(ctx context.Context, switchName string, portName string) error {
	// Look up the port to get its UUID
	lsp, err := c.GetLogicalSwitchPort(ctx, portName)
	if err != nil {
		return fmt.Errorf("get logical switch port for delete: %w", err)
	}

	// Look up the switch to get its UUID
	ls, err := c.GetLogicalSwitch(ctx, switchName)
	if err != nil {
		return fmt.Errorf("get logical switch for port delete: %w", err)
	}

	// Remove the port from the switch's ports set
	mutateOps, err := c.client.Where(ls).Mutate(ls, model.Mutation{
		Field:   &ls.Ports,
		Mutator: "delete",
		Value:   []string{lsp.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate logical switch ports ops: %w", err)
	}

	// Delete the port
	deleteOps, err := c.client.Where(lsp).Delete()
	if err != nil {
		return fmt.Errorf("delete logical switch port ops: %w", err)
	}

	ops := append(mutateOps, deleteOps...)
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("delete logical switch port transact: %w", err)
	}
	return nil
}

func (c *LiveOVNClient) GetLogicalSwitchPort(ctx context.Context, name string) (*nbdb.LogicalSwitchPort, error) {
	var ports []nbdb.LogicalSwitchPort
	err := c.client.WhereCache(func(lsp *nbdb.LogicalSwitchPort) bool {
		return lsp.Name == name
	}).List(ctx, &ports)
	if err != nil {
		return nil, fmt.Errorf("get logical switch port: %w", err)
	}
	if len(ports) == 0 {
		return nil, fmt.Errorf("logical switch port %q not found", name)
	}
	return &ports[0], nil
}

func (c *LiveOVNClient) UpdateLogicalSwitchPort(ctx context.Context, lsp *nbdb.LogicalSwitchPort) error {
	// Ensure we have the UUID for the Where clause
	if lsp.UUID == "" {
		existing, err := c.GetLogicalSwitchPort(ctx, lsp.Name)
		if err != nil {
			return fmt.Errorf("get logical switch port for update: %w", err)
		}
		lsp.UUID = existing.UUID
	}
	ops, err := c.client.Where(lsp).Update(lsp)
	if err != nil {
		return fmt.Errorf("update logical switch port ops: %w", err)
	}
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("update logical switch port transact: %w", err)
	}
	return nil
}

func (c *LiveOVNClient) CreateLogicalRouter(ctx context.Context, lr *nbdb.LogicalRouter) error {
	ops, err := c.client.Create(lr)
	if err != nil {
		return fmt.Errorf("create logical router ops: %w", err)
	}
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("create logical router transact: %w", err)
	}
	return nil
}

func (c *LiveOVNClient) DeleteLogicalRouter(ctx context.Context, name string) error {
	lr, err := c.GetLogicalRouter(ctx, name)
	if err != nil {
		return fmt.Errorf("delete logical router lookup: %w", err)
	}
	ops, err := c.client.Where(lr).Delete()
	if err != nil {
		return fmt.Errorf("delete logical router ops: %w", err)
	}
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("delete logical router transact: %w", err)
	}
	return nil
}

func (c *LiveOVNClient) GetLogicalRouter(ctx context.Context, name string) (*nbdb.LogicalRouter, error) {
	var routers []nbdb.LogicalRouter
	err := c.client.WhereCache(func(lr *nbdb.LogicalRouter) bool {
		return lr.Name == name
	}).List(ctx, &routers)
	if err != nil {
		return nil, fmt.Errorf("get logical router: %w", err)
	}
	if len(routers) == 0 {
		return nil, fmt.Errorf("logical router %q not found", name)
	}
	return &routers[0], nil
}

func (c *LiveOVNClient) ListLogicalRouters(ctx context.Context) ([]nbdb.LogicalRouter, error) {
	var routers []nbdb.LogicalRouter
	err := c.client.List(ctx, &routers)
	if err != nil {
		return nil, fmt.Errorf("list logical routers: %w", err)
	}
	return routers, nil
}

func (c *LiveOVNClient) CreateLogicalRouterPort(ctx context.Context, routerName string, lrp *nbdb.LogicalRouterPort) error {
	// Set a named UUID so the port can be referenced in the same transaction
	if lrp.UUID == "" {
		lrp.UUID = namedUUID("lrp_", lrp.Name)
	}

	createOps, err := c.client.Create(lrp)
	if err != nil {
		return fmt.Errorf("create logical router port ops: %w", err)
	}

	// Look up the router to get its UUID for the Where clause
	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return fmt.Errorf("get logical router for port add: %w", err)
	}

	mutateOps, err := c.client.Where(lr).Mutate(lr, model.Mutation{
		Field:   &lr.Ports,
		Mutator: "insert",
		Value:   []string{lrp.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate logical router ports ops: %w", err)
	}

	ops := append(createOps, mutateOps...)
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("create logical router port transact: %w", err)
	}
	return nil
}

func (c *LiveOVNClient) DeleteLogicalRouterPort(ctx context.Context, routerName string, portName string) error {
	// Look up the port to get its UUID
	lrp, err := c.GetLogicalRouterPort(ctx, portName)
	if err != nil {
		return fmt.Errorf("get logical router port for delete: %w", err)
	}

	// Look up the router to get its UUID
	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return fmt.Errorf("get logical router for port delete: %w", err)
	}

	mutateOps, err := c.client.Where(lr).Mutate(lr, model.Mutation{
		Field:   &lr.Ports,
		Mutator: "delete",
		Value:   []string{lrp.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate logical router ports ops: %w", err)
	}

	deleteOps, err := c.client.Where(lrp).Delete()
	if err != nil {
		return fmt.Errorf("delete logical router port ops: %w", err)
	}

	ops := append(mutateOps, deleteOps...)
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("delete logical router port transact: %w", err)
	}
	return nil
}

func (c *LiveOVNClient) GetLogicalRouterPort(ctx context.Context, name string) (*nbdb.LogicalRouterPort, error) {
	var ports []nbdb.LogicalRouterPort
	err := c.client.WhereCache(func(lrp *nbdb.LogicalRouterPort) bool {
		return lrp.Name == name
	}).List(ctx, &ports)
	if err != nil {
		return nil, fmt.Errorf("get logical router port: %w", err)
	}
	if len(ports) == 0 {
		return nil, fmt.Errorf("logical router port %q not found", name)
	}
	return &ports[0], nil
}

// UpdateLogicalRouterPort rewrites mutable columns on an existing LRP.
// Used by ensureGatewayPortNetworks to retrofit the link-local Networks
// CIDR onto gateway ports created by older code that used pool IPs
// (mulga-siv-26 D8). Mirrors UpdateLogicalSwitchPort.
func (c *LiveOVNClient) UpdateLogicalRouterPort(ctx context.Context, lrp *nbdb.LogicalRouterPort) error {
	if lrp.UUID == "" {
		existing, err := c.GetLogicalRouterPort(ctx, lrp.Name)
		if err != nil {
			return fmt.Errorf("get logical router port for update: %w", err)
		}
		lrp.UUID = existing.UUID
	}
	ops, err := c.client.Where(lrp).Update(lrp)
	if err != nil {
		return fmt.Errorf("update logical router port ops: %w", err)
	}
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("update logical router port transact: %w", err)
	}
	return nil
}

// SetGatewayChassis binds a chassis to an LRP for HA gateway scheduling. Read-
// then-decide so it tolerates re-runs of reconcile/IGW-attach: absent → create
// + LRP-mutate; present + same priority → no-op; present + different priority
// → mutate priority on the existing row. Required for the reconcile-time
// rebind step that recovers from chassis_name drift (mulga-999).
func (c *LiveOVNClient) SetGatewayChassis(ctx context.Context, lrpName string, chassisName string, priority int) error {
	gcName := lrpName + "-" + chassisName
	existing, err := c.GetGatewayChassisByName(ctx, gcName)
	if err != nil {
		return fmt.Errorf("get existing gateway chassis: %w", err)
	}
	if existing != nil {
		if existing.Priority == priority {
			return nil
		}
		return c.updateGatewayChassisPriority(ctx, existing, priority)
	}

	lrp, err := c.GetLogicalRouterPort(ctx, lrpName)
	if err != nil {
		return fmt.Errorf("get logical router port for gateway chassis: %w", err)
	}

	gc := &nbdb.GatewayChassis{
		UUID:        namedUUID("gc_", gcName),
		Name:        gcName,
		ChassisName: chassisName,
		Priority:    priority,
		ExternalIDs: map[string]string{},
		Options:     map[string]string{},
	}

	createOps, err := c.client.Create(gc)
	if err != nil {
		return fmt.Errorf("create gateway chassis ops: %w", err)
	}

	mutateOps, err := c.client.Where(lrp).Mutate(lrp, model.Mutation{
		Field:   &lrp.GatewayChassis,
		Mutator: "insert",
		Value:   []string{gc.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate logical router port gateway_chassis ops: %w", err)
	}

	ops := append(createOps, mutateOps...)
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("set gateway chassis transact: %w", err)
	}
	return nil
}

func (c *LiveOVNClient) updateGatewayChassisPriority(ctx context.Context, gc *nbdb.GatewayChassis, priority int) error {
	gc.Priority = priority
	ops, err := c.client.Where(gc).Update(gc, &gc.Priority)
	if err != nil {
		return fmt.Errorf("update gateway_chassis priority ops: %w", err)
	}
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("update gateway_chassis priority transact: %w", err)
	}
	return nil
}

// GetGatewayChassisByName looks up a Gateway_Chassis row by its `name` column
// (the deterministic "lrpName-chassisName" form). Returns (nil, nil) when no
// row matches — matches the cache-lookup convention used by GetLogicalSwitch
// et al.
func (c *LiveOVNClient) GetGatewayChassisByName(ctx context.Context, name string) (*nbdb.GatewayChassis, error) {
	var rows []nbdb.GatewayChassis
	err := c.client.WhereCache(func(gc *nbdb.GatewayChassis) bool {
		return gc.Name == name
	}).List(ctx, &rows)
	if err != nil {
		return nil, fmt.Errorf("list gateway_chassis by name: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// ListGatewayChassis returns every Gateway_Chassis row. The reconcile loop
// uses this to find rows referencing chassis names that no longer exist in
// the SBDB (e.g. because the OVS system-id changed across a reboot —
// mulga-999).
func (c *LiveOVNClient) ListGatewayChassis(ctx context.Context) ([]nbdb.GatewayChassis, error) {
	var rows []nbdb.GatewayChassis
	if err := c.client.List(ctx, &rows); err != nil {
		return nil, fmt.Errorf("list gateway_chassis: %w", err)
	}
	return rows, nil
}

// DeleteGatewayChassis removes a Gateway_Chassis row and detaches it from the
// owning LRP in one transaction. lrpName is required so the mutation targets
// the correct router port; gcUUID identifies the row to remove.
func (c *LiveOVNClient) DeleteGatewayChassis(ctx context.Context, lrpName string, gcUUID string) error {
	lrp, err := c.GetLogicalRouterPort(ctx, lrpName)
	if err != nil {
		return fmt.Errorf("get logical router port for gateway chassis delete: %w", err)
	}

	mutateOps, err := c.client.Where(lrp).Mutate(lrp, model.Mutation{
		Field:   &lrp.GatewayChassis,
		Mutator: "delete",
		Value:   []string{gcUUID},
	})
	if err != nil {
		return fmt.Errorf("mutate logical router port gateway_chassis (delete) ops: %w", err)
	}

	gc := &nbdb.GatewayChassis{UUID: gcUUID}
	deleteOps, err := c.client.Where(gc).Delete()
	if err != nil {
		return fmt.Errorf("delete gateway_chassis ops: %w", err)
	}

	ops := append(mutateOps, deleteOps...)
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("delete gateway_chassis transact: %w", err)
	}
	return nil
}

// ListLogicalRouterPorts returns every LRP across all routers. Used by the
// reconcile-time gateway-chassis rebind to find every LRP tagged
// external_ids:spinifex:role=gateway (mulga-999).
func (c *LiveOVNClient) ListLogicalRouterPorts(ctx context.Context) ([]nbdb.LogicalRouterPort, error) {
	var rows []nbdb.LogicalRouterPort
	if err := c.client.List(ctx, &rows); err != nil {
		return nil, fmt.Errorf("list logical_router_port: %w", err)
	}
	return rows, nil
}

func (c *LiveOVNClient) CreateDHCPOptions(ctx context.Context, opts *nbdb.DHCPOptions) (string, error) {
	ops, err := c.client.Create(opts)
	if err != nil {
		return "", fmt.Errorf("create DHCP options ops: %w", err)
	}
	results, err := c.client.Transact(ctx, ops...)
	if err != nil {
		return "", fmt.Errorf("create DHCP options transact: %w", err)
	}
	if _, err := ovsdb.CheckOperationResults(results, ops); err != nil {
		return "", fmt.Errorf("create DHCP options check: %w", err)
	}
	if len(results) > 0 {
		return results[0].UUID.GoUUID, nil
	}
	return "", nil
}

func (c *LiveOVNClient) DeleteDHCPOptions(ctx context.Context, uuid string) error {
	opts := &nbdb.DHCPOptions{UUID: uuid}
	ops, err := c.client.Where(opts).Delete()
	if err != nil {
		return fmt.Errorf("delete DHCP options ops: %w", err)
	}
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("delete DHCP options transact: %w", err)
	}
	return nil
}

func (c *LiveOVNClient) FindDHCPOptionsByCIDR(ctx context.Context, cidr string) (*nbdb.DHCPOptions, error) {
	var options []nbdb.DHCPOptions
	err := c.client.WhereCache(func(o *nbdb.DHCPOptions) bool {
		return o.CIDR == cidr
	}).List(ctx, &options)
	if err != nil {
		return nil, fmt.Errorf("find DHCP options by CIDR: %w", err)
	}
	if len(options) == 0 {
		return nil, fmt.Errorf("DHCP options for CIDR %q not found", cidr)
	}
	return &options[0], nil
}

func (c *LiveOVNClient) FindDHCPOptionsByExternalID(ctx context.Context, key, value string) (*nbdb.DHCPOptions, error) {
	var options []nbdb.DHCPOptions
	err := c.client.WhereCache(func(o *nbdb.DHCPOptions) bool {
		return o.ExternalIDs[key] == value
	}).List(ctx, &options)
	if err != nil {
		return nil, fmt.Errorf("find DHCP options by external_id %s=%s: %w", key, value, err)
	}
	if len(options) == 0 {
		return nil, fmt.Errorf("DHCP options with external_id %s=%s not found", key, value)
	}
	return &options[0], nil
}

func (c *LiveOVNClient) ListDHCPOptions(ctx context.Context) ([]nbdb.DHCPOptions, error) {
	var options []nbdb.DHCPOptions
	err := c.client.List(ctx, &options)
	if err != nil {
		return nil, fmt.Errorf("list DHCP options: %w", err)
	}
	return options, nil
}

func (c *LiveOVNClient) AddNAT(ctx context.Context, routerName string, nat *nbdb.NAT) error {
	// Set a named UUID so the NAT can be referenced in the same transaction
	if nat.UUID == "" {
		nat.UUID = namedUUID("nat_", nat.Type+"_"+nat.LogicalIP)
	}

	createOps, err := c.client.Create(nat)
	if err != nil {
		return fmt.Errorf("create NAT ops: %w", err)
	}

	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return fmt.Errorf("get logical router for NAT add: %w", err)
	}

	mutateOps, err := c.client.Where(lr).Mutate(lr, model.Mutation{
		Field:   &lr.NAT,
		Mutator: "insert",
		Value:   []string{nat.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate router NAT ops: %w", err)
	}

	ops := append(createOps, mutateOps...)
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("add NAT transact: %w", err)
	}
	return nil
}

func (c *LiveOVNClient) DeleteNAT(ctx context.Context, routerName string, natType, logicalIP string) error {
	// Find the NAT entry by type and logical IP
	var nats []nbdb.NAT
	err := c.client.WhereCache(func(n *nbdb.NAT) bool {
		return n.Type == natType && n.LogicalIP == logicalIP
	}).List(ctx, &nats)
	if err != nil {
		return fmt.Errorf("find NAT: %w", err)
	}
	if len(nats) == 0 {
		return fmt.Errorf("NAT %s %s not found", natType, logicalIP)
	}

	nat := &nats[0]
	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return fmt.Errorf("get logical router for NAT delete: %w", err)
	}

	mutateOps, err := c.client.Where(lr).Mutate(lr, model.Mutation{
		Field:   &lr.NAT,
		Mutator: "delete",
		Value:   []string{nat.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate router NAT ops: %w", err)
	}

	deleteOps, err := c.client.Where(nat).Delete()
	if err != nil {
		return fmt.Errorf("delete NAT ops: %w", err)
	}

	ops := append(mutateOps, deleteOps...)
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("delete NAT transact: %w", err)
	}
	return nil
}

// DeleteNATByExternalIP removes a NAT rule matching the given external IP.
// Returns an error if no matching rule is found (callers can ignore this).
func (c *LiveOVNClient) DeleteNATByExternalIP(ctx context.Context, routerName string, natType, externalIP string) error {
	var nats []nbdb.NAT
	err := c.client.WhereCache(func(n *nbdb.NAT) bool {
		return n.Type == natType && n.ExternalIP == externalIP
	}).List(ctx, &nats)
	if err != nil {
		return fmt.Errorf("find NAT by external IP: %w", err)
	}
	if len(nats) == 0 {
		return fmt.Errorf("NAT %s external_ip=%s not found", natType, externalIP)
	}

	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return fmt.Errorf("get logical router for NAT delete: %w", err)
	}

	var allOps []ovsdb.Operation
	for i := range nats {
		nat := &nats[i]
		mutateOps, mErr := c.client.Where(lr).Mutate(lr, model.Mutation{
			Field:   &lr.NAT,
			Mutator: "delete",
			Value:   []string{nat.UUID},
		})
		if mErr != nil {
			return fmt.Errorf("mutate router NAT ops: %w", mErr)
		}
		deleteOps, dErr := c.client.Where(nat).Delete()
		if dErr != nil {
			return fmt.Errorf("delete NAT ops: %w", dErr)
		}
		allOps = append(allOps, mutateOps...)
		allOps = append(allOps, deleteOps...)
	}

	if err := c.transactOps(ctx, allOps); err != nil {
		return fmt.Errorf("delete NAT by external IP transact: %w", err)
	}
	return nil
}

// DeleteAllNATsByExternalIP removes all NAT rules matching the given external
// IP from every router that references them. This handles cross-VPC stale NAT
// rules that remain when vpc.delete-nat (fire-and-forget) hasn't been processed
// before an IP is reused by a different VPC. Returns the number of rules deleted.
func (c *LiveOVNClient) DeleteAllNATsByExternalIP(ctx context.Context, natType, externalIP string) (int, error) {
	var nats []nbdb.NAT
	if err := c.client.WhereCache(func(n *nbdb.NAT) bool {
		return n.Type == natType && n.ExternalIP == externalIP
	}).List(ctx, &nats); err != nil {
		return 0, fmt.Errorf("find NAT by external IP: %w", err)
	}
	if len(nats) == 0 {
		return 0, nil
	}

	routers, err := c.ListLogicalRouters(ctx)
	if err != nil {
		return 0, fmt.Errorf("list routers for stale NAT cleanup: %w", err)
	}

	staleUUIDs := make(map[string]struct{}, len(nats))
	for _, n := range nats {
		staleUUIDs[n.UUID] = struct{}{}
	}

	// Build a single transaction: remove NAT refs from all routers, then delete NAT rows.
	var allOps []ovsdb.Operation
	for i := range routers {
		lr := &routers[i]
		for _, natUUID := range lr.NAT {
			if _, stale := staleUUIDs[natUUID]; !stale {
				continue
			}
			ops, err := c.client.Where(lr).Mutate(lr, model.Mutation{
				Field: &lr.NAT, Mutator: "delete", Value: []string{natUUID},
			})
			if err != nil {
				return 0, fmt.Errorf("mutate router %s: %w", lr.Name, err)
			}
			allOps = append(allOps, ops...)
		}
	}
	for i := range nats {
		ops, err := c.client.Where(&nats[i]).Delete()
		if err != nil {
			return 0, fmt.Errorf("delete NAT row: %w", err)
		}
		allOps = append(allOps, ops...)
	}
	if err := c.transactOps(ctx, allOps); err != nil {
		return 0, fmt.Errorf("delete all NATs by external IP: %w", err)
	}
	return len(nats), nil
}

// FindNATByExternalIP returns the first NAT rule matching the given type and
// external IP, or nil if none exists. Used by the startup reconcile to check
// whether an EIP already has a dnat_and_snat rule before (re-)creating one.
func (c *LiveOVNClient) FindNATByExternalIP(ctx context.Context, natType, externalIP string) (*nbdb.NAT, error) {
	var nats []nbdb.NAT
	if err := c.client.WhereCache(func(n *nbdb.NAT) bool {
		return n.Type == natType && n.ExternalIP == externalIP
	}).List(ctx, &nats); err != nil {
		return nil, fmt.Errorf("find NAT by external IP: %w", err)
	}
	if len(nats) == 0 {
		return nil, nil
	}
	return &nats[0], nil
}

func (c *LiveOVNClient) AddStaticRoute(ctx context.Context, routerName string, route *nbdb.LogicalRouterStaticRoute) error {
	// Set a named UUID so the route can be referenced in the same transaction
	if route.UUID == "" {
		route.UUID = namedUUID("route_", route.IPPrefix)
	}

	createOps, err := c.client.Create(route)
	if err != nil {
		return fmt.Errorf("create static route ops: %w", err)
	}

	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return fmt.Errorf("get logical router for route add: %w", err)
	}

	mutateOps, err := c.client.Where(lr).Mutate(lr, model.Mutation{
		Field:   &lr.StaticRoutes,
		Mutator: "insert",
		Value:   []string{route.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate router static routes ops: %w", err)
	}

	ops := append(createOps, mutateOps...)
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("add static route transact: %w", err)
	}
	return nil
}

func (c *LiveOVNClient) DeleteStaticRoute(ctx context.Context, routerName string, ipPrefix string) error {
	// Find the route by IP prefix
	var routes []nbdb.LogicalRouterStaticRoute
	err := c.client.WhereCache(func(r *nbdb.LogicalRouterStaticRoute) bool {
		return r.IPPrefix == ipPrefix
	}).List(ctx, &routes)
	if err != nil {
		return fmt.Errorf("find static route: %w", err)
	}
	if len(routes) == 0 {
		return fmt.Errorf("static route %s not found", ipPrefix)
	}

	route := &routes[0]
	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return fmt.Errorf("get logical router for route delete: %w", err)
	}

	mutateOps, err := c.client.Where(lr).Mutate(lr, model.Mutation{
		Field:   &lr.StaticRoutes,
		Mutator: "delete",
		Value:   []string{route.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate router static routes ops: %w", err)
	}

	deleteOps, err := c.client.Where(route).Delete()
	if err != nil {
		return fmt.Errorf("delete static route ops: %w", err)
	}

	ops := append(mutateOps, deleteOps...)
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("delete static route transact: %w", err)
	}
	return nil
}

// Port Groups

func (c *LiveOVNClient) CreatePortGroup(ctx context.Context, name string, ports []string) error {
	pg := &nbdb.PortGroup{
		UUID:        namedUUID("pg_", name),
		Name:        name,
		Ports:       ports,
		ExternalIDs: map[string]string{},
	}
	ops, err := c.client.Create(pg)
	if err != nil {
		return fmt.Errorf("create port group ops: %w", err)
	}
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("create port group transact: %w", err)
	}
	return nil
}

func (c *LiveOVNClient) DeletePortGroup(ctx context.Context, name string) error {
	pg, err := c.getPortGroup(ctx, name)
	if err != nil {
		return fmt.Errorf("delete port group lookup: %w", err)
	}
	ops, err := c.client.Where(pg).Delete()
	if err != nil {
		return fmt.Errorf("delete port group ops: %w", err)
	}
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("delete port group transact: %w", err)
	}
	return nil
}

func (c *LiveOVNClient) UpdatePortGroupMemberships(ctx context.Context, lspName, privateIP string, addPGs, removePGs []string) error {
	if len(addPGs) == 0 && len(removePGs) == 0 {
		return nil
	}
	lsp, err := c.GetLogicalSwitchPort(ctx, lspName)
	if err != nil {
		return fmt.Errorf("update port group memberships lsp lookup: %w", err)
	}

	var ops []ovsdb.Operation
	appendPGOps := func(pgName, mutator string) error {
		pg, err := c.getPortGroup(ctx, pgName)
		if err != nil {
			return fmt.Errorf("port group %s lookup: %w", pgName, err)
		}
		pgOps, err := c.client.Where(pg).Mutate(pg, model.Mutation{
			Field:   &pg.Ports,
			Mutator: ovsdb.Mutator(mutator),
			Value:   []string{lsp.UUID},
		})
		if err != nil {
			return fmt.Errorf("mutate port group %s ops: %w", pgName, err)
		}
		ops = append(ops, pgOps...)

		if privateIP == "" {
			return nil
		}
		asName := addressSetName(pgName)
		as, err := c.getAddressSet(ctx, asName)
		if err != nil {
			return fmt.Errorf("address set %s lookup: %w", asName, err)
		}
		asOps, err := c.client.Where(as).Mutate(as, model.Mutation{
			Field:   &as.Addresses,
			Mutator: ovsdb.Mutator(mutator),
			Value:   []string{privateIP},
		})
		if err != nil {
			return fmt.Errorf("mutate address set %s ops: %w", asName, err)
		}
		ops = append(ops, asOps...)
		return nil
	}

	for _, pgName := range addPGs {
		if err := appendPGOps(pgName, "insert"); err != nil {
			return err
		}
	}
	for _, pgName := range removePGs {
		if err := appendPGOps(pgName, "delete"); err != nil {
			return err
		}
	}

	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("update port group memberships transact: %w", err)
	}
	return nil
}

func (c *LiveOVNClient) getPortGroup(ctx context.Context, name string) (*nbdb.PortGroup, error) {
	var pgs []nbdb.PortGroup
	err := c.client.WhereCache(func(pg *nbdb.PortGroup) bool {
		return pg.Name == name
	}).List(ctx, &pgs)
	if err != nil {
		return nil, fmt.Errorf("get port group: %w", err)
	}
	if len(pgs) == 0 {
		return nil, fmt.Errorf("port group %q not found", name)
	}
	return &pgs[0], nil
}

// GetPortGroup is the exported wrapper around getPortGroup. Returns an error
// when the named port group is not present in the cache.
func (c *LiveOVNClient) GetPortGroup(ctx context.Context, name string) (*nbdb.PortGroup, error) {
	return c.getPortGroup(ctx, name)
}

func (c *LiveOVNClient) ListPortGroups(ctx context.Context) ([]nbdb.PortGroup, error) {
	var pgs []nbdb.PortGroup
	if err := c.client.List(ctx, &pgs); err != nil {
		return nil, fmt.Errorf("list port groups: %w", err)
	}
	return pgs, nil
}

// ListPortGroupsForPort scans the cache for port groups whose Ports set
// contains the given LSP's UUID. Returns the names. Returns an empty slice
// (not an error) when the LSP has no memberships, so reconcilePortSGs handles
// the "not currently in any group" case naturally.
func (c *LiveOVNClient) ListPortGroupsForPort(ctx context.Context, lspName string) ([]string, error) {
	lsp, err := c.GetLogicalSwitchPort(ctx, lspName)
	if err != nil {
		return nil, fmt.Errorf("list port groups for port lookup: %w", err)
	}
	var pgs []nbdb.PortGroup
	if err := c.client.List(ctx, &pgs); err != nil {
		return nil, fmt.Errorf("list port groups: %w", err)
	}
	names := make([]string, 0)
	for i := range pgs {
		if slices.Contains(pgs[i].Ports, lsp.UUID) {
			names = append(names, pgs[i].Name)
		}
	}
	return names, nil
}

// ACLs

func (c *LiveOVNClient) AddACLs(ctx context.Context, portGroupName string, specs []ACLSpec) error {
	if len(specs) == 0 {
		return nil
	}
	pg, err := c.getPortGroup(ctx, portGroupName)
	if err != nil {
		return fmt.Errorf("add ACLs port group lookup: %w", err)
	}

	var ops []ovsdb.Operation
	uuids := make([]string, 0, len(specs))
	for i, spec := range specs {
		// Index disambiguates ACLs that collapse to the same (direction,match)
		// after sanitisation — e.g. a default-deny egress and a 0.0.0.0/0
		// allow egress both produce "inport == @pg && ip4". Without the
		// index, OVSDB rejects the second insert with "duplicate uuid-name".
		acl := &nbdb.ACL{
			UUID:        namedUUID("acl_", fmt.Sprintf("%s_%d_%s", portGroupName, i, spec.Direction)),
			Direction:   spec.Direction,
			Priority:    spec.Priority,
			Match:       spec.Match,
			Action:      spec.Action,
			Log:         spec.Log,
			ExternalIDs: map[string]string{},
		}
		if spec.Name != "" {
			name := spec.Name
			acl.Name = &name
		}
		if spec.Severity != "" {
			severity := spec.Severity
			acl.Severity = &severity
		}
		createOps, err := c.client.Create(acl)
		if err != nil {
			return fmt.Errorf("create ACL ops: %w", err)
		}
		ops = append(ops, createOps...)
		uuids = append(uuids, acl.UUID)
	}

	mutateOps, err := c.client.Where(pg).Mutate(pg, model.Mutation{
		Field:   &pg.ACLs,
		Mutator: ovsdb.MutateOperationInsert,
		Value:   uuids,
	})
	if err != nil {
		return fmt.Errorf("mutate port group ACLs ops: %w", err)
	}
	ops = append(ops, mutateOps...)

	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("add ACLs transact: %w", err)
	}
	return nil
}

// ClearACLs removes every ACL row referenced by the port group and detaches
// them from the port group's ACLs set in one transaction.
func (c *LiveOVNClient) ClearACLs(ctx context.Context, portGroupName string) error {
	pg, err := c.getPortGroup(ctx, portGroupName)
	if err != nil {
		return fmt.Errorf("clear ACLs port group lookup: %w", err)
	}
	if len(pg.ACLs) == 0 {
		return nil
	}

	mutateOps, err := c.client.Where(pg).Mutate(pg, model.Mutation{
		Field:   &pg.ACLs,
		Mutator: ovsdb.MutateOperationDelete,
		Value:   pg.ACLs,
	})
	if err != nil {
		return fmt.Errorf("mutate port group ACLs (delete) ops: %w", err)
	}

	ops := mutateOps
	for _, aclUUID := range pg.ACLs {
		acl := &nbdb.ACL{UUID: aclUUID}
		deleteOps, dErr := c.client.Where(acl).Delete()
		if dErr != nil {
			return fmt.Errorf("delete ACL ops: %w", dErr)
		}
		ops = append(ops, deleteOps...)
	}

	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("clear ACLs transact: %w", err)
	}
	return nil
}

// Address Sets

func (c *LiveOVNClient) CreateAddressSet(ctx context.Context, name string, addresses []string) error {
	as := &nbdb.AddressSet{
		UUID:        namedUUID("as_", name),
		Name:        name,
		Addresses:   addresses,
		ExternalIDs: map[string]string{},
	}
	ops, err := c.client.Create(as)
	if err != nil {
		return fmt.Errorf("create address set ops: %w", err)
	}
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("create address set transact: %w", err)
	}
	return nil
}

func (c *LiveOVNClient) DeleteAddressSet(ctx context.Context, name string) error {
	as, err := c.getAddressSet(ctx, name)
	if err != nil {
		return fmt.Errorf("delete address set lookup: %w", err)
	}
	ops, err := c.client.Where(as).Delete()
	if err != nil {
		return fmt.Errorf("delete address set ops: %w", err)
	}
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("delete address set transact: %w", err)
	}
	return nil
}

// AddAddressSetEntry is idempotent: re-inserting an existing address is a
// no-op in OVSDB.
func (c *LiveOVNClient) AddAddressSetEntry(ctx context.Context, name string, address string) error {
	return c.mutateAddressSetEntries(ctx, name, address, "insert")
}

func (c *LiveOVNClient) RemoveAddressSetEntry(ctx context.Context, name string, address string) error {
	return c.mutateAddressSetEntries(ctx, name, address, "delete")
}

func (c *LiveOVNClient) mutateAddressSetEntries(ctx context.Context, name, address, mutator string) error {
	as, err := c.getAddressSet(ctx, name)
	if err != nil {
		return fmt.Errorf("address set %s lookup: %w", mutator, err)
	}
	ops, err := c.client.Where(as).Mutate(as, model.Mutation{
		Field:   &as.Addresses,
		Mutator: ovsdb.Mutator(mutator),
		Value:   []string{address},
	})
	if err != nil {
		return fmt.Errorf("mutate address set entries ops: %w", err)
	}
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("mutate address set entries transact: %w", err)
	}
	return nil
}

func (c *LiveOVNClient) getAddressSet(ctx context.Context, name string) (*nbdb.AddressSet, error) {
	var sets []nbdb.AddressSet
	err := c.client.WhereCache(func(as *nbdb.AddressSet) bool {
		return as.Name == name
	}).List(ctx, &sets)
	if err != nil {
		return nil, fmt.Errorf("get address set: %w", err)
	}
	if len(sets) == 0 {
		return nil, fmt.Errorf("address set %q not found", name)
	}
	return &sets[0], nil
}
