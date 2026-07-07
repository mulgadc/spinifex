// Package nbdb contains libovsdb model structs for the OVN Northbound tables
// used by Spinifex VPC networking.
package nbdb

import "github.com/ovn-kubernetes/libovsdb/model"

// LogicalSwitch represents an OVN Logical_Switch (L2 segment, maps to a subnet).
type LogicalSwitch struct {
	UUID        string            `ovsdb:"_uuid"`
	Name        string            `ovsdb:"name"`
	Ports       []string          `ovsdb:"ports"`
	ACLs        []string          `ovsdb:"acls"`
	DNSRecords  []string          `ovsdb:"dns_records"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
	OtherConfig map[string]string `ovsdb:"other_config"`
}

// LogicalSwitchPort represents an OVN Logical_Switch_Port (VM port / ENI).
type LogicalSwitchPort struct {
	UUID          string            `ovsdb:"_uuid"`
	Name          string            `ovsdb:"name"`
	Type          string            `ovsdb:"type"`
	Addresses     []string          `ovsdb:"addresses"`
	PortSecurity  []string          `ovsdb:"port_security"`
	DHCPv4Options *string           `ovsdb:"dhcpv4_options"`
	Enabled       *bool             `ovsdb:"enabled"`
	Up            *bool             `ovsdb:"up"`
	ExternalIDs   map[string]string `ovsdb:"external_ids"`
	Options       map[string]string `ovsdb:"options"`
}

// LogicalRouter represents an OVN Logical_Router (VPC router).
type LogicalRouter struct {
	UUID         string            `ovsdb:"_uuid"`
	Name         string            `ovsdb:"name"`
	Ports        []string          `ovsdb:"ports"`
	StaticRoutes []string          `ovsdb:"static_routes"`
	NAT          []string          `ovsdb:"nat"`
	Policies     []string          `ovsdb:"policies"`
	Enabled      *bool             `ovsdb:"enabled"`
	ExternalIDs  map[string]string `ovsdb:"external_ids"`
	Options      map[string]string `ovsdb:"options"`
}

// LogicalRouterPort represents an OVN Logical_Router_Port.
type LogicalRouterPort struct {
	UUID           string            `ovsdb:"_uuid"`
	Name           string            `ovsdb:"name"`
	MAC            string            `ovsdb:"mac"`
	Networks       []string          `ovsdb:"networks"`
	GatewayChassis []string          `ovsdb:"gateway_chassis"`
	ExternalIDs    map[string]string `ovsdb:"external_ids"`
	Options        map[string]string `ovsdb:"options"`
}

// DHCPOptions represents an OVN DHCP_Options row.
type DHCPOptions struct {
	UUID        string            `ovsdb:"_uuid"`
	CIDR        string            `ovsdb:"cidr"`
	Options     map[string]string `ovsdb:"options"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

// NAT represents an OVN NAT rule on a Logical_Router.
type NAT struct {
	UUID        string  `ovsdb:"_uuid"`
	Type        string  `ovsdb:"type"` // "snat", "dnat", "dnat_and_snat"
	ExternalIP  string  `ovsdb:"external_ip"`
	LogicalIP   string  `ovsdb:"logical_ip"`
	LogicalPort *string `ovsdb:"logical_port"`
	ExternalMAC *string `ovsdb:"external_mac"`
	// ExemptedExtIps references an Address_Set (strong ref); packets whose
	// destination (snat) / source (dnat) matches the set skip this NAT rule.
	ExemptedExtIps *string           `ovsdb:"exempted_ext_ips"`
	ExternalIDs    map[string]string `ovsdb:"external_ids"`
	Options        map[string]string `ovsdb:"options"`
}

// LogicalRouterStaticRoute represents an OVN Logical_Router_Static_Route.
type LogicalRouterStaticRoute struct {
	UUID        string            `ovsdb:"_uuid"`
	IPPrefix    string            `ovsdb:"ip_prefix"`
	Nexthop     string            `ovsdb:"nexthop"`
	OutputPort  *string           `ovsdb:"output_port"`
	Policy      *string           `ovsdb:"policy"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

// LogicalRouterPolicy represents an OVN Logical_Router_Policy row attached
// to Logical_Router.policies. Used for per-subnet egress steering: the match
// expression typically combines an `inport == "rtr-<subnetID>"` filter with
// a destination predicate, and the action reroutes via a chosen nexthop.
type LogicalRouterPolicy struct {
	UUID        string            `ovsdb:"_uuid"`
	Priority    int               `ovsdb:"priority"`
	Match       string            `ovsdb:"match"`
	Action      string            `ovsdb:"action"` // "allow", "drop", "reroute"
	Nexthop     *string           `ovsdb:"nexthop"`
	Nexthops    []string          `ovsdb:"nexthops"`
	Options     map[string]string `ovsdb:"options"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

// PortGroup represents an OVN Port_Group (used for SG enforcement).
type PortGroup struct {
	UUID        string            `ovsdb:"_uuid"`
	Name        string            `ovsdb:"name"`
	Ports       []string          `ovsdb:"ports"` // UUIDs of logical switch ports
	ACLs        []string          `ovsdb:"acls"`  // UUIDs of ACLs
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

// ACL represents an OVN ACL rule attached to a port group or logical switch.
type ACL struct {
	UUID        string            `ovsdb:"_uuid"`
	Name        *string           `ovsdb:"name"`
	Direction   string            `ovsdb:"direction"` // "to-lport" or "from-lport"
	Priority    int               `ovsdb:"priority"`
	Match       string            `ovsdb:"match"`
	Action      string            `ovsdb:"action"` // "allow-related", "drop", "allow", "reject"
	Log         bool              `ovsdb:"log"`
	Severity    *string           `ovsdb:"severity"` // "alert", "warning", "notice", "info", "debug"
	Meter       *string           `ovsdb:"meter"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

// AddressSet represents an OVN Address_Set referenced by ACL match exprs
// (e.g. ip4.src == $sg_xxx_ip4) for SG-to-SG rules.
type AddressSet struct {
	UUID        string            `ovsdb:"_uuid"`
	Name        string            `ovsdb:"name"`
	Addresses   []string          `ovsdb:"addresses"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

// GatewayChassis represents an OVN Gateway_Chassis for HA scheduling.
type GatewayChassis struct {
	UUID        string            `ovsdb:"_uuid"`
	Name        string            `ovsdb:"name"`
	ChassisName string            `ovsdb:"chassis_name"`
	Priority    int               `ovsdb:"priority"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
	Options     map[string]string `ovsdb:"options"`
}

// FullDatabaseModel returns a ClientDBModel for the OVN NB tables used.
func FullDatabaseModel() (model.ClientDBModel, error) {
	return model.NewClientDBModel("OVN_Northbound", map[string]model.Model{
		"Logical_Switch":              &LogicalSwitch{},
		"Logical_Switch_Port":         &LogicalSwitchPort{},
		"Logical_Router":              &LogicalRouter{},
		"Logical_Router_Port":         &LogicalRouterPort{},
		"DHCP_Options":                &DHCPOptions{},
		"NAT":                         &NAT{},
		"Logical_Router_Static_Route": &LogicalRouterStaticRoute{},
		"Logical_Router_Policy":       &LogicalRouterPolicy{},
		"Gateway_Chassis":             &GatewayChassis{},
		"Port_Group":                  &PortGroup{},
		"ACL":                         &ACL{},
		"Address_Set":                 &AddressSet{},
	})
}
