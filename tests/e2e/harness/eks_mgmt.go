//go:build e2e

package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// ControlPlaneMgmtIP resolves the EKS control-plane VM's host-bridge (br-mgmt)
// address — the only address reachable from the host/runner. The cluster
// endpoint in DescribeCluster is the internal NLB DNS, which is NXDOMAIN here
// and whose :443->:6443 passthrough is not wired; the VPC NIC (172.31.x) is
// likewise unreachable. The mgmt IP is persisted unencrypted on the cluster's
// KV meta record (ClusterMeta.ControlPlaneMgmtIP), so read it directly over
// NATS. This is the Phase-2 reachability shim — see the eks-6g-e2e plan; the
// real fix (NLB/Route53 passthrough) is tracked separately.
func ControlPlaneMgmtIP(t *testing.T, env *Env, accountID, clusterName string) string {
	t.Helper()
	host, token, ca := natsConn(t, env)
	nc, err := utils.ConnectNATS(host, token, ca)
	if err != nil {
		t.Fatalf("connect NATS %s: %v", host, err)
	}
	defer nc.Close()

	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.KeyValue(handlers_eks.AccountBucketName(accountID))
	if err != nil {
		t.Fatalf("open EKS account KV for %s: %v", accountID, err)
	}
	meta, err := handlers_eks.GetClusterMeta(kv, clusterName)
	if err != nil {
		t.Fatalf("get cluster meta %s: %v", clusterName, err)
	}
	if meta.ControlPlaneMgmtIP == "" {
		t.Fatalf("cluster %s has no ControlPlaneMgmtIP recorded", clusterName)
	}
	t.Logf("control-plane mgmt IP: %s", meta.ControlPlaneMgmtIP)
	return meta.ControlPlaneMgmtIP
}

// natsConn resolves the NATS host/token/CA for the harness's own JetStream
// reads. SPINIFEX_NATS_* env overrides win (CI / runner-resident runs); else it
// reads the local node's stanza from spinifex.toml.
func natsConn(t *testing.T, env *Env) (host, token, ca string) {
	t.Helper()
	host = os.Getenv("SPINIFEX_NATS_URL")
	token = os.Getenv("SPINIFEX_NATS_TOKEN")
	ca = os.Getenv("SPINIFEX_NATS_CA")
	if host != "" {
		return host, token, ca
	}

	cfgPath := filepath.Join(env.ConfigDir, "spinifex.toml")
	cc, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load %s for NATS settings: %v", cfgPath, err)
	}
	node := nodeConfig(cc)
	if node == nil {
		t.Fatalf("no node stanza with a NATS host in %s", cfgPath)
	}
	return node.NATS.Host, node.NATS.ACL.Token, node.NATS.CACert
}

// nodeConfig returns the local node's Config (cc.Node), falling back to the
// first node carrying a NATS host.
func nodeConfig(cc *config.ClusterConfig) *config.Config {
	if cc == nil {
		return nil
	}
	if n, ok := cc.Nodes[cc.Node]; ok && n.NATS.Host != "" {
		return &n
	}
	for _, n := range cc.Nodes {
		if n.NATS.Host != "" {
			return &n
		}
	}
	return nil
}
