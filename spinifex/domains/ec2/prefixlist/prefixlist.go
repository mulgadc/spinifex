// Package prefixlist serves the AWS-managed prefix list for the S3 gateway
// endpoint service. The list is derived on every read from the node set, so it
// has no record and no version.
package prefixlist

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/netip"
	"strconv"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
)

// DefaultGatePort is Predastore's S3 port when a node's config names none.
const DefaultGatePort = 8443

// OwnerID is the owner AWS renders for an AWS-managed prefix list.
const OwnerID = "AWS"

// ServiceName is the S3 endpoint service in region, in AWS's literal form.
func ServiceName(region string) string { return "com.amazonaws." + region + ".s3" }

// ID is the S3 list's identifier: stable for a region, and the same on every node.
func ID(region string) string {
	sum := sha256.Sum256([]byte(region + "|" + ServiceName(region)))
	return "pl-" + hex.EncodeToString(sum[:4])
}

// ARN is the list's ARN, owned by the account AWS renders as "aws".
func ARN(region string) string {
	return "arn:aws:ec2:" + region + ":aws:prefix-list/" + ID(region)
}

// Entry is one node address in the list and the S3 port it serves on. The port
// is used by the datapath only and is never rendered.
type Entry struct {
	Addr     netip.Addr
	GatePort uint16
}

// CIDR renders the entry as AWS lists it.
func (e Entry) CIDR() string { return netip.PrefixFrom(e.Addr, 32).String() }

// Entries derives the list from the gossiped node set: one IPv4 /32 per node
// service address. Every node passes no br-mgmt address, so every node, and
// the API and the datapath, derive the same list.
func Entries(cc *config.ClusterConfig) []Entry {
	var out []Entry
	for _, ep := range config.ServiceEndpoints(cc, "") {
		if !ep.Addr.Is4() {
			continue
		}
		out = append(out, Entry{Addr: ep.Addr, GatePort: gatePort(cc.Nodes[ep.Node])})
	}
	return out
}

// gatePort is the port of the node's Predastore host, or the default.
func gatePort(node config.Config) uint16 {
	_, port, err := net.SplitHostPort(node.Predastore.Host)
	if err != nil {
		return DefaultGatePort
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return DefaultGatePort
	}
	return uint16(n)
}
