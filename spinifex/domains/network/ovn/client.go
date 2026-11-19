// Package ovn is L1 of the spinifex network stack: the OVN Northbound DB
// client interface and implementations. Higher layers (topology, policy,
// external, federation) must call OVN only through this interface.
package ovn

import (
	"context"
	"errors"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
)

// ErrNATNotFound is returned when a NAT rule is absent (use errors.Is for idempotency).
var ErrNATNotFound = errors.New("NAT not found")

// ErrPortGroupNotFound is returned when a port group is absent (use errors.Is for idempotency).
var ErrPortGroupNotFound = errors.New("port group not found")

// ErrAddressSetNotFound is returned when an address set is absent (use errors.Is for idempotency).
var ErrAddressSetNotFound = errors.New("address set not found")

// ErrLogicalRouterNotFound is returned when a logical router is absent (use errors.Is).
var ErrLogicalRouterNotFound = errors.New("logical router not found")

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

// ExternalIDPcxID stamps every row a VPC peering connection owns: its link
// ports, guards, entry routes, no-route drops and reroutes. A route's drop does not.
const ExternalIDPcxID = "spinifex:pcx_id"

// ExternalIDRole names what a spinifex-owned row is for.
const ExternalIDRole = "spinifex:role"

// SamePeeringOwner reports whether two rows' external IDs name the same
// connection and role; a row with no role matches only another with none.
func SamePeeringOwner(a, b map[string]string) bool {
	return a[ExternalIDPcxID] == b[ExternalIDPcxID] && a[ExternalIDRole] == b[ExternalIDRole]
}

// PeeringGuardRole is the spinifex:role a peering guard policy carries, so a
// re-ensure can find the connection's other guards on a router.
const PeeringGuardRole = "peering-guard"

// PeeringNoRouteRole is the spinifex:role of the drop that decides traffic to
// the peer range wherever no peering route does.
const PeeringNoRouteRole = "peering-no-route"

// PeeringLinkEnd is one router's end of a VPC peering link: the port that
// joins it to the peer router, the guard on traffic arriving through it, and
// the entry route and no-route drop that let the policy stage decide the peer range.
type PeeringLinkEnd struct {
	Router  string
	Port    nbdb.LogicalRouterPort
	Guard   nbdb.LogicalRouterPolicy
	Entry   nbdb.LogicalRouterStaticRoute
	NoRoute nbdb.LogicalRouterPolicy
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
	UpdateDHCPOptionsOptions(ctx context.Context, uuid string, options map[string]string) error
	DeleteDHCPOptions(ctx context.Context, uuid string) error
	FindDHCPOptionsByExternalID(ctx context.Context, key, value string) (*nbdb.DHCPOptions, error)
	ListDHCPOptions(ctx context.Context) ([]nbdb.DHCPOptions, error)

	// NAT rules
	AddNAT(ctx context.Context, routerName string, nat *nbdb.NAT) error
	DeleteNAT(ctx context.Context, routerName string, natType, logicalIP string) error
	DeleteNATByExternalIP(ctx context.Context, routerName string, natType, externalIP string) error
	DeleteAllNATsByExternalIP(ctx context.Context, natType, externalIP string) (int, error)
	FindNATByExternalIP(ctx context.Context, natType, externalIP string) (*nbdb.NAT, error)
	FindNATByLogicalIP(ctx context.Context, routerName, natType, logicalIP string) (*nbdb.NAT, error)
	// ListNATs returns every NAT row in OVN NB. The reconciler uses it to prune
	// orphan dnat_and_snat rows whose stamped owning ENI is gone from intent.
	ListNATs(ctx context.Context) ([]nbdb.NAT, error)
	// SetNATExemptedExtIPs updates exempted_ext_ips in place on the NAT rule
	// matching (natType, logicalIP) on routerName — no delete/re-add flow gap.
	// nil clears the ref. Returns ErrNATNotFound when the rule is absent.
	SetNATExemptedExtIPs(ctx context.Context, routerName, natType, logicalIP string, addressSetUUID *string) error

	// Address Sets (referenced by NAT exempted_ext_ips and ACL match exprs)
	// EnsureAddressSet atomically creates the named address set or converges the
	// addresses on the existing row (see EnsureLogicalRouter for the wait-op
	// rationale). Returns the persisted row UUID.
	EnsureAddressSet(ctx context.Context, name string, addresses []string) (uuid string, err error)
	// CreateAddressSetIfAbsent inserts the named set empty unless it exists. It
	// never reads or writes an existing set's addresses, including when it
	// loses the insert to a concurrent writer.
	CreateAddressSetIfAbsent(ctx context.Context, name string) (uuid string, err error)
	GetAddressSet(ctx context.Context, name string) (*nbdb.AddressSet, error)

	// Static routes
	AddStaticRoute(ctx context.Context, routerName string, route *nbdb.LogicalRouterStaticRoute) error
	DeleteStaticRoute(ctx context.Context, routerName string, ipPrefix string) error
	FindStaticRoute(ctx context.Context, routerName, ipPrefix string) (*nbdb.LogicalRouterStaticRoute, error)
	ListStaticRoutes(ctx context.Context, routerName string) ([]nbdb.LogicalRouterStaticRoute, error)

	// Logical Router Policies (per-subnet egress steering). Identity is the
	// (router, priority, match) triple — same triple replaces, missing rows
	// on delete return nil (mirrors AddStaticRoute / DeleteStaticRoute).
	AddLogicalRouterPolicy(ctx context.Context, routerName string, policy *nbdb.LogicalRouterPolicy) error
	DeleteLogicalRouterPolicy(ctx context.Context, routerName string, priority int, match string) error
	FindLogicalRouterPolicy(ctx context.Context, routerName string, priority int, match string) (*nbdb.LogicalRouterPolicy, error)
	ListLogicalRouterPolicies(ctx context.Context, routerName string) ([]nbdb.LogicalRouterPolicy, error)
	// UpdateLogicalRouterPolicy rewrites the action, next hops, options and
	// external IDs of the row with policy's priority and match in one write,
	// so the row is never absent. A missing row is an error.
	UpdateLogicalRouterPolicy(ctx context.Context, routerName string, policy *nbdb.LogicalRouterPolicy) error

	// EnsurePeeringLink writes both ends' rows in one transaction, creating each
	// or rewriting it in place, and removes the connection's other rows of the
	// same kind on either router. A failed transaction changes nothing.
	EnsurePeeringLink(ctx context.Context, ends [2]PeeringLinkEnd) error
	// DeletePeeringLink removes every port, policy and static route stamped pcxID on the
	// named routers in one transaction. A router absent from NB is skipped.
	DeletePeeringLink(ctx context.Context, pcxID string, routers []string) error

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
	// ListACLs returns every ACL row in the client's view.
	ListACLs(ctx context.Context) ([]nbdb.ACL, error)

	// Gateway Chassis (HA scheduling for gateway router ports)
	SetGatewayChassis(ctx context.Context, lrpName string, chassisName string, priority int) error
	GetGatewayChassisByName(ctx context.Context, name string) (*nbdb.GatewayChassis, error)
	ListGatewayChassis(ctx context.Context) ([]nbdb.GatewayChassis, error)
	DeleteGatewayChassis(ctx context.Context, lrpName string, gcUUID string) error
}
