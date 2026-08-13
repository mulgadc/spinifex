//go:build e2e

package multinode

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/listenerinventory"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// runListenerInvariant is the runtime half of the listener invariant. The
// static half in spinifex/network/invariants reads install scripts and
// config templates; only this half reads what each node's kernel actually
// bound, which is what caught the original OVN NB/SB wildcard defect (spinifex
// #765) — that bug was found by hand on a live node with `ss -tulnp`, not by
// any static test.
//
// docs/security/network-connections/README.md "## 1. Inbound Listeners" is
// the same fixture the static half reads: it classifies each port's intended
// reach, and a row's own prose is the only thing allowed to excuse a
// wildcard bind. No port list is hardcoded here.
func runListenerInvariant(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Listener Invariant")

	root, err := listenerinventory.RepoRoot()
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	table, err := listenerinventory.ParseFile(listenerinventory.DocPath(root))
	if err != nil {
		t.Fatalf("parse inbound listener inventory: %v", err)
	}

	ssh := harness.NewPeerSSH()
	var violations []string
	for _, node := range fix.Cluster.Nodes {
		harness.Step(t, "ss -tulnp on %s", node.Name)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		out, runErr := ssh.Run(ctx, node.Addr, "sudo ss -tulnp")
		cancel()
		if runErr != nil {
			t.Fatalf("ss -tulnp on %s: %v", node.Name, runErr)
		}

		for _, sock := range parseSSOutput(string(out)) {
			for _, r := range table.RowsForPort(sock.port) {
				if r.Scope != listenerinventory.ScopeCluster && r.Scope != listenerinventory.ScopeEncap {
					continue
				}
				if v := checkSocketScope(node, sock, r); v != "" {
					violations = append(violations, v)
				}
			}
		}
	}

	if len(violations) > 0 {
		t.Fatalf("listener invariant violated (docs/security/network-connections/README.md"+
			" \"## 1. Inbound Listeners\" is the fixture; add or correct a row rather than"+
			" widening this test):\n  %s", strings.Join(violations, "\n  "))
	}
	harness.Detail(t, "nodes_checked", len(fix.Cluster.Nodes))
}

// checkSocketScope reports a violation message if sock is bound somewhere
// row's Scope does not permit, honoring row's own doc-declared wildcard
// exception. Returns "" if the socket is clean for this row.
func checkSocketScope(node harness.Node, sock ssSocket, row listenerinventory.Row) string {
	wildcard := listenerinventory.IsWildcardAddr(sock.addr)
	switch {
	case wildcard && row.WildcardOK():
		return ""
	case wildcard:
		return fmt.Sprintf(
			"%s: port %d (%s, %s scope) bound to the wildcard address %s; its inventory row does not declare a wildcard exception",
			node.Name, sock.port, sock.proto, row.Scope, sock.addr)
	case sock.addr == node.Addr:
		return fmt.Sprintf(
			"%s: port %d (%s, %s scope) bound to %s, the node's WAN address",
			node.Name, sock.port, sock.proto, row.Scope, sock.addr)
	}
	return ""
}

// ssSocket is one row of `ss -tulnp` output: a listening TCP socket or an
// unconnected (server) UDP socket.
type ssSocket struct {
	proto string
	addr  string
	port  int
}

// parseSSOutput extracts every tcp/udp local bind from `ss -tulnp` output.
// Columns are whitespace-separated except the Process field, which is a
// single comma-joined token with no embedded space.
func parseSSOutput(out string) []ssSocket {
	var socks []ssSocket
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		proto := fields[0]
		if proto != "tcp" && proto != "udp" {
			continue // header row, or a protocol ss -tulnp does not report
		}
		addr, portStr := splitAddrPort(fields[4])
		port, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		socks = append(socks, ssSocket{proto: proto, addr: addr, port: port})
	}
	return socks
}

// splitAddrPort splits an ss(8) "Local Address:Port" token into address and
// port, handling the bracketed IPv6 form ("[::]:500") separately since an
// IPv6 address contains colons of its own.
func splitAddrPort(tok string) (addr, port string) {
	if strings.HasPrefix(tok, "[") {
		end := strings.Index(tok, "]")
		if end < 0 {
			return tok, ""
		}
		return tok[1:end], strings.TrimPrefix(tok[end+1:], ":")
	}
	i := strings.LastIndex(tok, ":")
	if i < 0 {
		return tok, ""
	}
	return tok[:i], tok[i+1:]
}
