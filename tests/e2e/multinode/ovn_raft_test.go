//go:build e2e

package multinode

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/host"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

const (
	ovnNBSock   = "/var/run/ovn/ovnnb_db.ctl"
	ovnSBSock   = "/var/run/ovn/ovnsb_db.ctl"
	ovnNBSchema = "OVN_Northbound"
	ovnSBSchema = "OVN_Southbound"
)

// runOVNRaft asserts the clustered OVN control plane: every DB node's NB and SB
// RAFT cluster reports the full quorum with exactly one leader, then kills the
// NB leader's ovn-central and proves NB writes still land via a surviving
// endpoint (libovsdb-style multi-endpoint failover, here through ovn-nbctl).
func runOVNRaft(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — OVN OVSDB RAFT")

	require.GreaterOrEqualf(t, len(fix.Cluster.Nodes), 3,
		"OVN RAFT quorum requires a 3-node cluster, have %d", len(fix.Cluster.Nodes))
	// The first three nodes host the clustered NB/SB DBs (mirrors
	// formation.BuildOVNDBAddrs); in a 3-node cluster that is every node.
	dbNodes := fix.Cluster.Nodes[:3]
	ssh := harness.NewPeerSSH()

	// 1. Quorum health: 3 servers + exactly one leader, per schema, per node.
	for _, db := range []struct{ name, sock, schema string }{
		{"NB", ovnNBSock, ovnNBSchema},
		{"SB", ovnSBSock, ovnSBSchema},
	} {
		leaders := 0
		for _, n := range dbNodes {
			harness.Step(t, "%s cluster/status on %s (%s)", db.name, n.Name, n.Addr)
			servers, role := ovnClusterStatus(t, ssh, n, db.sock, db.schema)
			harness.Detail(t, fmt.Sprintf("%s_%s_servers", db.name, n.Name), servers)
			harness.Detail(t, fmt.Sprintf("%s_%s_role", db.name, n.Name), role)
			require.Equalf(t, 3, servers, "%s on %s: want 3 servers in quorum", db.name, n.Name)
			if role == "leader" {
				leaders++
			}
		}
		require.Equalf(t, 1, leaders, "%s: want exactly one leader across the quorum", db.name)
	}

	// 2. Failover: kill the NB leader and confirm an NB write still succeeds.
	var leader harness.Node
	var survivors []harness.Node
	for _, n := range dbNodes {
		if _, role := ovnClusterStatus(t, ssh, n, ovnNBSock, ovnNBSchema); role == "leader" {
			leader = n
		} else {
			survivors = append(survivors, n)
		}
	}
	require.NotEmptyf(t, leader.Name, "no NB leader found across quorum")
	require.NotEmptyf(t, survivors, "no surviving DB node to write through")

	harness.Step(t, "stop ovn-central on NB leader %s (%s)", leader.Name, leader.Addr)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	_, err := ssh.Run(stopCtx, leader.Addr,
		"sudo systemctl stop ovn-central ovn-ovsdb-server-nb ovn-ovsdb-server-sb ovn-northd")
	require.NoErrorf(t, err, "stop ovn-central on %s", leader.Name)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, e := ssh.Run(ctx, leader.Addr,
			"sudo systemctl start ovn-ovsdb-server-nb ovn-ovsdb-server-sb ovn-northd"); e != nil {
			t.Logf("cleanup: restart ovn-central on %s: %v", leader.Name, e)
		}
	})

	// The --db list includes the dead leader so the write must fail over past it.
	dbList := nbEndpointList(dbNodes)
	sw := fmt.Sprintf("e2e-raft-failover-%d", time.Now().Unix())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = ssh.Run(ctx, survivors[0].Addr,
			fmt.Sprintf("sudo ovn-nbctl --db=%s --timeout=10 --if-exists ls-del %s", dbList, sw))
	})

	harness.Step(t, "NB write (ls-add %s) via %s after leader loss", sw, survivors[0].Name)
	// RAFT re-election after the leader drops takes a beat; retry the write
	// until a new leader accepts it or the deadline passes.
	var writeErr error
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		_, writeErr = ssh.Run(ctx, survivors[0].Addr,
			fmt.Sprintf("sudo ovn-nbctl --db=%s --timeout=20 ls-add %s", dbList, sw))
		cancel()
		if writeErr == nil {
			break
		}
		time.Sleep(3 * time.Second)
	}
	require.NoErrorf(t, writeErr, "NB ls-add via surviving endpoint after leader %s loss", leader.Name)

	harness.Step(t, "confirm %s is present in NB via surviving endpoint", sw)
	readCtx, readCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer readCancel()
	out, err := ssh.Run(readCtx, survivors[0].Addr,
		fmt.Sprintf("sudo ovn-nbctl --db=%s --timeout=15 ls-list", dbList))
	require.NoError(t, err, "NB ls-list via surviving endpoint")
	require.Containsf(t, string(out), sw, "switch %s not visible after failover write", sw)
}

// ovnClusterStatus runs cluster/status for one schema on a node and returns the
// reported server count and the queried server's role.
func ovnClusterStatus(t *testing.T, ssh *harness.PeerSSH, n harness.Node, sock, schema string) (servers int, role string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := ssh.Run(ctx, n.Addr, fmt.Sprintf("sudo ovn-appctl -t %s cluster/status %s", sock, schema))
	require.NoErrorf(t, err, "cluster/status %s on %s", schema, n.Name)
	return host.ParseClusterStatus(string(out))
}

// nbEndpointList builds the comma-separated NB client endpoint list used by
// ovn-nbctl --db across the given DB nodes.
func nbEndpointList(nodes []harness.Node) string {
	eps := make([]string, len(nodes))
	for i, n := range nodes {
		eps[i] = fmt.Sprintf("tcp:%s:6641", n.Addr)
	}
	return strings.Join(eps, ",")
}
