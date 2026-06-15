//go:build e2e

package harness

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Node identifies a single cluster member addressable over SSH and HTTPS.
// Index is 1-based; scenarios reference nodes by index so ordering is fixed.
type Node struct {
	Index int    // 1-based position in the cluster
	Name  string // human-readable tag, e.g. "node1"
	Addr  string // IP or hostname; used for SSH and as peer address for partition rules
}

// Cluster is an ordered set of Nodes plus shared SSH credentials.
type Cluster struct {
	Nodes      []Node
	SSHUser    string
	SSHKeyPath string
}

// Peers returns every node in the cluster except target. The result is used
// by fault.PartitionNode to build per-peer iptables DROP rules.
func (c *Cluster) Peers(target Node) []Node {
	out := make([]Node, 0, len(c.Nodes)-1)
	for _, n := range c.Nodes {
		if n.Index == target.Index {
			continue
		}
		out = append(out, n)
	}
	return out
}

// ClusterFromEnv builds a Cluster from SPINIFEX_NODES / SPINIFEX_SSH_USER /
// SPINIFEX_SSH_KEY. Returns an error if any required variable is missing.
func ClusterFromEnv() (*Cluster, error) {
	nodesRaw := envWithLegacy("SPINIFEX_NODES", "DDIL_NODES")
	user := envWithLegacy("SPINIFEX_SSH_USER", "DDIL_SSH_USER")
	key := envWithLegacy("SPINIFEX_SSH_KEY", "DDIL_SSH_KEY")

	var missing []string
	if nodesRaw == "" {
		missing = append(missing, "SPINIFEX_NODES")
	}
	if user == "" {
		missing = append(missing, "SPINIFEX_SSH_USER")
	}
	if key == "" {
		missing = append(missing, "SPINIFEX_SSH_KEY")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("e2e harness: missing required env: %s", strings.Join(missing, ", "))
	}

	var nodes []Node
	for i, addr := range strings.Split(nodesRaw, ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		nodes = append(nodes, Node{
			Index: i + 1,
			Name:  fmt.Sprintf("node%d", i+1),
			Addr:  addr,
		})
	}
	if len(nodes) == 0 {
		return nil, errors.New("e2e harness: SPINIFEX_NODES contained no addresses")
	}

	return &Cluster{
		Nodes:      nodes,
		SSHUser:    user,
		SSHKeyPath: key,
	}, nil
}

// NodeFromEnv returns the Nth node (1-based) from the cluster in SPINIFEX_NODES.
func NodeFromEnv(index int) (Node, error) {
	c, err := ClusterFromEnv()
	if err != nil {
		return Node{}, err
	}
	if index < 1 || index > len(c.Nodes) {
		return Node{}, fmt.Errorf("e2e harness: node index %d out of range (have %d nodes)", index, len(c.Nodes))
	}
	return c.Nodes[index-1], nil
}

// envWithLegacy returns os.Getenv(canonical), falling back to legacy on miss.
// A stderr note flags use of the deprecated DDIL_* names.
func envWithLegacy(canonical, legacy string) string {
	if v := os.Getenv(canonical); v != "" {
		return v
	}
	if v := os.Getenv(legacy); v != "" {
		fmt.Fprintf(os.Stderr, "e2e harness: %s is deprecated; set %s instead\n", legacy, canonical)
		return v
	}
	return ""
}
