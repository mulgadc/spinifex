package config

import (
	"net"
	"net/netip"
	"sort"
)

// GatewayHostForNode returns the host a node serves AWS service endpoints on.
// Precedence: mgmt-dedicated AWSGW IP, AdvertiseIP, br-mgmt IP, DevNetworking,
// then the AWSGW bind IP; "" when none applies.
func GatewayHostForNode(node Config, mgmtBridgeIP string) string {
	awsgwBindIP := ""
	if node.AWSGW.Host != "" {
		if h, _, splitErr := net.SplitHostPort(node.AWSGW.Host); splitErr == nil {
			awsgwBindIP = h
		}
	}
	advertiseIP := node.AdvertiseIP

	switch {
	case mgmtBridgeIP != "" && awsgwBindIP != "" && awsgwBindIP != "0.0.0.0" &&
		!net.ParseIP(awsgwBindIP).IsLoopback() && awsgwBindIP != advertiseIP:
		return awsgwBindIP
	case advertiseIP != "" && advertiseIP != "0.0.0.0":
		return advertiseIP
	case mgmtBridgeIP != "":
		return mgmtBridgeIP
	case node.Daemon.DevNetworking:
		return "10.0.2.2"
	case awsgwBindIP != "" && awsgwBindIP != "0.0.0.0":
		return awsgwBindIP
	}
	return ""
}

// ServiceEndpoint is one node address that serves the AWS service endpoints,
// and the node that owns it.
type ServiceEndpoint struct {
	Addr netip.Addr
	Node string
}

// ServiceEndpoints returns every node's service address from the gossiped
// node set, sorted by address with duplicates dropped. localMgmtBridgeIP
// applies to cc.Node only, since a peer's br-mgmt address is never gossiped.
func ServiceEndpoints(cc *ClusterConfig, localMgmtBridgeIP string) []ServiceEndpoint {
	if cc == nil {
		return nil
	}
	names := make([]string, 0, len(cc.Nodes))
	for name := range cc.Nodes {
		names = append(names, name)
	}
	sort.Strings(names)

	seen := map[netip.Addr]bool{}
	var out []ServiceEndpoint
	for _, name := range names {
		mgmt := ""
		if name == cc.Node {
			mgmt = localMgmtBridgeIP
		}
		// A non-IP host has no address record to publish, so it is skipped.
		addr, err := netip.ParseAddr(GatewayHostForNode(cc.Nodes[name], mgmt))
		if err != nil {
			continue
		}
		addr = addr.Unmap()
		if seen[addr] {
			continue
		}
		seen[addr] = true
		out = append(out, ServiceEndpoint{Addr: addr, Node: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr.Less(out[j].Addr) })
	return out
}
