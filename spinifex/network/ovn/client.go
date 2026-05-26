// Package ovn is L1 of the spinifex network stack: the OVN Northbound DB
// client interface and implementations. Higher layers (topology, policy,
// external, federation) must call OVN only through this interface.
package ovn

import (
	"context"
	"errors"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
)

// ErrNATNotFound is returned by DeleteNAT / DeleteNATByExternalIP when the
// rule is already absent. Callers that want idempotent semantics (e.g. the
// vpc.delete-nat NATS handler, which races with vm-cleanup paths that emit
// the same delete) match it with errors.Is and treat as success.
var ErrNATNotFound = errors.New("NAT not found")

// ErrPortGroupNotFound is returned by GetPortGroup when the named port group
// is absent. EnsureSGPortGroup / DeleteSGPortGroup use errors.Is to make
// existence checks and delete-then-recheck flows idempotent.
var ErrPortGroupNotFound = errors.New("port group not found")

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

// Client is the OVN Northbound DB abstraction. The vpcd in-tree
// implementation is LiveClient (live.go); tests use network/ovn/mock.
type Client interface {
	// Connection lifecycle
	Connect(ctx context.Context) error
	Close()
	Connected() bool

	// Logical Switch (subnet)
	CreateLogicalSwitch(ctx context.Context, ls *nbdb.LogicalSwitch) error
	// EnsureLogicalSwitch atomically creates a logical switch if no row with
	// the same Name already exists, or returns the existing row. Wraps the
	// insert in an OVSDB wait-op that asserts "zero rows match Name == ls.Name"
	// at commit time; concurrent writers across nodes are serialised by
	// ovsdb-server rather than by the local libovsdb cache (which can be
	// stale relative to another node's just-committed insert). See
	// EnsureLogicalRouter for the rationale.
	EnsureLogicalSwitch(ctx context.Context, ls *nbdb.LogicalSwitch) (*nbdb.LogicalSwitch, error)
	DeleteLogicalSwitch(ctx context.Context, name string) error
	GetLogicalSwitch(ctx context.Context, name string) (*nbdb.LogicalSwitch, error)
	ListLogicalSwitches(ctx context.Context) ([]nbdb.LogicalSwitch, error)

	// Logical Switch Port (VM/ENI)
	CreateLogicalSwitchPort(ctx context.Context, switchName string, lsp *nbdb.LogicalSwitchPort) error
	// CreateLogicalSwitchPortInGroups creates an LSP, adds it to its switch,
	// and joins it to the named port groups — all in a single OVSDB
	// transaction. Required for SG enforcement: a non-atomic create-then-join
	// leaves a window where the LSP exists outside any port group (OVN
	// default = unrestricted). portGroupNames may be empty (e.g. router/
	// localnet ports). The per-port-group `_ip4`/`_ip6` Address_Set rows in
	// SB are auto-derived by ovn-northd from each port group's port
	// addresses; SG-to-SG match expressions resolve against those.
	CreateLogicalSwitchPortInGroups(ctx context.Context, switchName string, lsp *nbdb.LogicalSwitchPort, portGroupNames []string) error
	DeleteLogicalSwitchPort(ctx context.Context, switchName string, portName string) error
	GetLogicalSwitchPort(ctx context.Context, name string) (*nbdb.LogicalSwitchPort, error)
	UpdateLogicalSwitchPort(ctx context.Context, lsp *nbdb.LogicalSwitchPort) error

	// Logical Router (VPC router)
	CreateLogicalRouter(ctx context.Context, lr *nbdb.LogicalRouter) error
	// EnsureLogicalRouter atomically creates a logical router if no row with
	// the same Name already exists, or returns the existing row. The insert
	// is bundled with an OVSDB wait-op that asserts "zero rows match
	// Name == lr.Name" at commit time, so two writers (different goroutines
	// or different nodes) that both observe absence cannot both succeed —
	// ovsdb-server rejects the second transaction. The returned pointer is
	// the canonical row: newly created with the supplied fields, or the
	// pre-existing row when a competing writer raced us.
	//
	// Required because OVN NB has no unique constraint on Name. Without the
	// wait-op, the cross-handler / cross-node race between vpc.create and
	// vpc.create-subnet's defensive EnsureVPC call produced duplicate routers
	// per VPC (bead mulga-siv-146).
	EnsureLogicalRouter(ctx context.Context, lr *nbdb.LogicalRouter) (*nbdb.LogicalRouter, error)
	DeleteLogicalRouter(ctx context.Context, name string) error
	GetLogicalRouter(ctx context.Context, name string) (*nbdb.LogicalRouter, error)
	ListLogicalRouters(ctx context.Context) ([]nbdb.LogicalRouter, error)
	// UpdateLogicalRouterExternalIDs rewrites the ExternalIDs map of an
	// existing logical router. Used by topology.EnsureVPC to backfill the
	// `spinifex:cidr` / `spinifex:vni` keys when the row was first created by
	// a defensive EnsureVPC call that did not carry the parent VPC's full
	// metadata. The whole map is replaced; callers must pass the merged set.
	UpdateLogicalRouterExternalIDs(ctx context.Context, name string, externalIDs map[string]string) error

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
	FindStaticRoute(ctx context.Context, routerName, ipPrefix string) (*nbdb.LogicalRouterStaticRoute, error)

	// Port Groups (security group enforcement)
	CreatePortGroup(ctx context.Context, name string, ports []string) error
	// EnsurePortGroup is the wait-op-protected create-or-get analogue of
	// CreatePortGroup. Returns the canonical row. See EnsureLogicalRouter for
	// the wait-op pattern and rationale.
	EnsurePortGroup(ctx context.Context, name string, ports []string) (*nbdb.PortGroup, error)
	DeletePortGroup(ctx context.Context, name string) error
	// UpdatePortGroupMemberships applies all port-group joins and leaves for a
	// single LSP in one atomic OVSDB transaction. Required by reconcilePortSGs
	// so a 5-SG → different-5-SG modify never exposes an intermediate state
	// with fewer port groups (which would let the OVN default = unrestricted
	// apply for the gap). The per-port-group `_ip4`/`_ip6` Address_Set rows
	// in SB are auto-derived by ovn-northd from each port group's port
	// addresses; no explicit address-set update is required here.
	UpdatePortGroupMemberships(ctx context.Context, lspName string, addPGs, removePGs []string) error
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

	// Gateway Chassis (HA scheduling for gateway router ports)
	SetGatewayChassis(ctx context.Context, lrpName string, chassisName string, priority int) error
	GetGatewayChassisByName(ctx context.Context, name string) (*nbdb.GatewayChassis, error)
	ListGatewayChassis(ctx context.Context) ([]nbdb.GatewayChassis, error)
	DeleteGatewayChassis(ctx context.Context, lrpName string, gcUUID string) error
}
