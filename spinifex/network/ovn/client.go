// Package ovn is L1 of the spinifex network stack: the OVN Northbound DB
// client interface and implementations. Higher layers (topology, policy,
// external, federation) must call OVN only through this interface.
package ovn

import (
	"context"
	"errors"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
)

// ErrNATNotFound is returned when a NAT rule is absent (use errors.Is for idempotency).
var ErrNATNotFound = errors.New("NAT not found")

// ErrPortGroupNotFound is returned when a port group is absent (use errors.Is for idempotency).
var ErrPortGroupNotFound = errors.New("port group not found")

// ErrAddressSetNotFound is returned when an address set is absent (use errors.Is for idempotency).
var ErrAddressSetNotFound = errors.New("address set not found")

// ACLSpec describes an OVN ACL rule for attachment to a port group.
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
	// EnsureLogicalSwitch atomically creates a logical switch or returns the existing
	// row. Uses an OVSDB wait-op (see EnsureLogicalRouter for rationale). The returned
	// row always carries the real persisted UUID; created reports whether this call
	// inserted the row (false when an existing row was reused).
	EnsureLogicalSwitch(ctx context.Context, ls *nbdb.LogicalSwitch) (row *nbdb.LogicalSwitch, created bool, err error)
	DeleteLogicalSwitch(ctx context.Context, name string) error
	GetLogicalSwitch(ctx context.Context, name string) (*nbdb.LogicalSwitch, error)
	ListLogicalSwitches(ctx context.Context) ([]nbdb.LogicalSwitch, error)

	// Logical Switch Port (VM/ENI)
	CreateLogicalSwitchPort(ctx context.Context, switchName string, lsp *nbdb.LogicalSwitchPort) error
	// CreateLogicalSwitchPortInGroups creates an LSP and joins it to port groups in
	// one transaction — prevents the window where the LSP exists outside any group
	// (OVN default = unrestricted). portGroupNames may be empty.
	CreateLogicalSwitchPortInGroups(ctx context.Context, switchName string, lsp *nbdb.LogicalSwitchPort, portGroupNames []string) error
	DeleteLogicalSwitchPort(ctx context.Context, switchName string, portName string) error
	GetLogicalSwitchPort(ctx context.Context, name string) (*nbdb.LogicalSwitchPort, error)
	UpdateLogicalSwitchPort(ctx context.Context, lsp *nbdb.LogicalSwitchPort) error
	// ListLogicalSwitchPorts returns every LSP in OVN NB. The reconciler uses it
	// to find orphan ENI ports (spinifex:eni_id with no matching intent).
	ListLogicalSwitchPorts(ctx context.Context) ([]nbdb.LogicalSwitchPort, error)

	// Logical Router (VPC router)
	CreateLogicalRouter(ctx context.Context, lr *nbdb.LogicalRouter) error
	// EnsureLogicalRouter atomically creates a logical router or returns the existing
	// row. OVSDB wait-op serialises concurrent writers — NB has no unique-Name
	// constraint, so without it concurrent vpc.create calls produce duplicates. The
	// returned row always carries the real persisted UUID; created reports whether
	// this call inserted the row.
	EnsureLogicalRouter(ctx context.Context, lr *nbdb.LogicalRouter) (row *nbdb.LogicalRouter, created bool, err error)
	DeleteLogicalRouter(ctx context.Context, name string) error
	GetLogicalRouter(ctx context.Context, name string) (*nbdb.LogicalRouter, error)
	ListLogicalRouters(ctx context.Context) ([]nbdb.LogicalRouter, error)
	// UpdateLogicalRouterExternalIDs replaces the ExternalIDs map on an existing
	// router. Callers must pass the full merged set.
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
	FindNATByLogicalIP(ctx context.Context, routerName, natType, logicalIP string) (*nbdb.NAT, error)
	// SetNATExemptedExtIPs updates exempted_ext_ips in place on the NAT rule
	// matching (natType, logicalIP) on routerName — no delete/re-add flow gap.
	// nil clears the ref. Returns ErrNATNotFound when the rule is absent.
	SetNATExemptedExtIPs(ctx context.Context, routerName, natType, logicalIP string, addressSetUUID *string) error

	// Address Sets (referenced by NAT exempted_ext_ips and ACL match exprs)
	// EnsureAddressSet atomically creates the named address set or converges the
	// addresses on the existing row (see EnsureLogicalRouter for the wait-op
	// rationale). Returns the persisted row UUID.
	EnsureAddressSet(ctx context.Context, name string, addresses []string) (uuid string, err error)
	GetAddressSet(ctx context.Context, name string) (*nbdb.AddressSet, error)

	// Static routes
	AddStaticRoute(ctx context.Context, routerName string, route *nbdb.LogicalRouterStaticRoute) error
	DeleteStaticRoute(ctx context.Context, routerName string, ipPrefix string) error
	FindStaticRoute(ctx context.Context, routerName, ipPrefix string) (*nbdb.LogicalRouterStaticRoute, error)

	// Logical Router Policies (per-subnet egress steering). Identity is the
	// (router, priority, match) triple — same triple replaces, missing rows
	// on delete return nil (mirrors AddStaticRoute / DeleteStaticRoute).
	AddLogicalRouterPolicy(ctx context.Context, routerName string, policy *nbdb.LogicalRouterPolicy) error
	DeleteLogicalRouterPolicy(ctx context.Context, routerName string, priority int, match string) error
	FindLogicalRouterPolicy(ctx context.Context, routerName string, priority int, match string) (*nbdb.LogicalRouterPolicy, error)
	ListLogicalRouterPolicies(ctx context.Context, routerName string) ([]nbdb.LogicalRouterPolicy, error)

	// Port Groups (security group enforcement)
	CreatePortGroup(ctx context.Context, name string, ports []string) error
	// EnsurePortGroup atomically creates a port group or returns the existing row.
	// See EnsureLogicalRouter for the wait-op rationale. The returned row always
	// carries the real persisted UUID; created reports whether this call inserted it.
	EnsurePortGroup(ctx context.Context, name string, ports []string) (row *nbdb.PortGroup, created bool, err error)
	DeletePortGroup(ctx context.Context, name string) error
	// UpdatePortGroupMemberships applies all port-group joins and leaves for an LSP
	// in one transaction, preventing an intermediate state with fewer groups
	// (OVN default = unrestricted).
	UpdatePortGroupMemberships(ctx context.Context, lspName string, addPGs, removePGs []string) error
	// ListPortGroupsForPort returns the names of every port group containing the LSP.
	ListPortGroupsForPort(ctx context.Context, lspName string) ([]string, error)
	// GetPortGroup returns the port group with the given name, or an error if absent.
	GetPortGroup(ctx context.Context, name string) (*nbdb.PortGroup, error)
	// ListPortGroups returns every port group in OVN NB.
	ListPortGroups(ctx context.Context) ([]nbdb.PortGroup, error)

	// ACLs (attached to port groups). AddACLs creates all rows and links them
	// to the port group in one transaction.
	AddACLs(ctx context.Context, portGroupName string, specs []ACLSpec) error
	ClearACLs(ctx context.Context, portGroupName string) error

	// ReplaceACLs atomically swaps the port group's ACL set in one transaction.
	// Use instead of ClearACLs+AddACLs to avoid a zero-ACL window (defaults to drop).
	ReplaceACLs(ctx context.Context, portGroupName string, specs []ACLSpec) error

	// Gateway Chassis (HA scheduling for gateway router ports)
	SetGatewayChassis(ctx context.Context, lrpName string, chassisName string, priority int) error
	GetGatewayChassisByName(ctx context.Context, name string) (*nbdb.GatewayChassis, error)
	ListGatewayChassis(ctx context.Context) ([]nbdb.GatewayChassis, error)
	DeleteGatewayChassis(ctx context.Context, lrpName string, gcUUID string) error
}
