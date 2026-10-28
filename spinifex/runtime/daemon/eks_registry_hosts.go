package daemon

import (
	"sort"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ecr"
)

// peerRegistryPort is the awsgw port the install template gives every node.
// Peer entries in the cluster config carry no awsgw bind, so it is assumed.
const peerRegistryPort = "9999"

// registryHosts returns the registry host:port each gateway puts in its
// repositoryUri, from the start-time cluster config.
func (d *Daemon) registryHosts() []string {
	return registryHostsForCluster(d.clusterConfig, d.config)
}

// registryHostsForCluster lists the local node first, then peers by name.
// Nodes with no concrete host are skipped; the parity name already covers them.
func registryHostsForCluster(cc *config.ClusterConfig, local *config.Config) []string {
	var hosts []string
	seen := map[string]bool{}
	add := func(host, port string) {
		if host == "" {
			return
		}
		uri := ecr.RegistryURIHost(host, port)
		if !seen[uri] {
			seen[uri] = true
			hosts = append(hosts, uri)
		}
	}

	if local != nil {
		add(ecr.RegistryAddress(local.AWSGW.Host, local.AdvertiseIP))
	}
	if cc == nil {
		return hosts
	}
	names := make([]string, 0, len(cc.Nodes))
	for name := range cc.Nodes {
		if name != cc.Node {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		host, _ := ecr.RegistryAddress("", cc.Nodes[name].Host)
		add(host, peerRegistryPort)
	}
	return hosts
}
