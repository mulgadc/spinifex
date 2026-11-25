package cli

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/vpcendpoint"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveDaemonStatus answers node's feature-level status from its real gate.
func serveDaemonStatus(t *testing.T, nc *nats.Conn, node string, g *featurelevel.Gate) {
	t.Helper()
	handle := g.ServeStatus(node, featurelevel.Build)
	sub, err := nc.Subscribe(featurelevel.StatusSubject(node), func(m *nats.Msg) {
		var req featurelevel.StatusRequest
		if json.Unmarshal(m.Data, &req) != nil {
			return
		}
		reply, err := handle(t.Context(), &req, "")
		if err != nil {
			_ = m.Respond(nil)
			return
		}
		out := nats.NewMsg("")
		out.Data, _ = json.Marshal(reply)
		out.Header.Set(natsmsg.NodeIDHeader, node)
		_ = m.RespondMsg(out)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// serveVpcdStatus answers node's endpoint proxy status as a vpcd that has
// published a view since the store's latest revision.
func serveVpcdStatus(t *testing.T, nc *nats.Conn, node string, store *featurelevel.Store) {
	t.Helper()
	sub, err := nc.Subscribe(projection.EndpointProxyStatusSubject(node), func(m *nats.Msg) {
		_, rev, err := store.Read(t.Context())
		st := projection.EndpointProxyStatus{Node: node, Revision: rev, ProbePassed: true, GateReady: true, Build: featurelevel.Build, LevelRevision: rev}
		if err != nil {
			st = projection.EndpointProxyStatus{Error: err.Error()}
		}
		data, _ := json.Marshal(st)
		_ = m.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// The registered level 2 drain refuses a rollback while an endpoint is live or
// a rule names the S3 prefix list, and the same command finishes it once both
// are gone.
func TestRollback_GatewayEndpointDrainHoldsUntilEndpointsAndPrefixListRulesAreGone(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "cluster-config-drain-it", History: 1})
	require.NoError(t, err)
	store := featurelevel.NewStore(featurelevel.LeaderKV(js, kv))
	nodes := []string{"n1", "n2"}
	_, adv, err := store.Advance(t.Context(), []featurelevel.Member{
		{Node: "n1", Level: 2, Current: true}, {Node: "n2", Level: 2, Current: true},
	})
	require.NoError(t, err)
	require.True(t, adv)
	for _, n := range nodes {
		serveDaemonStatus(t, nc, n, featurelevel.NewGate(store))
		serveVpcdStatus(t, nc, n, store)
	}

	endpoints, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: vpcendpoint.KVBucketVpcEndpoints})
	require.NoError(t, err)
	rec, err := json.Marshal(vpcendpoint.EndpointRecord{
		ID: "vpce-0a1b2c3d4e5f60718", AccountID: "000000000001", VpcID: "vpc-0a1b2c3d4e5f60718",
		State: vpcendpoint.StateAvailable, CreatedAt: time.Now(),
	})
	require.NoError(t, err)
	_, err = endpoints.Put(t.Context(), "000000000001.vpce-0a1b2c3d4e5f60718", rec)
	require.NoError(t, err)

	groups, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: ec2vpc.KVBucketSecurityGroups})
	require.NoError(t, err)
	sg, err := json.Marshal(ec2vpc.SecurityGroupRecord{GroupId: "sg-0a1b2c3d4e5f60718", EgressRules: []ec2vpc.SGRule{
		{IpProtocol: "tcp", FromPort: 443, ToPort: 443, PrefixListId: "pl-6ca54005"},
	}})
	require.NoError(t, err)
	_, err = groups.Put(t.Context(), "000000000001.sg-0a1b2c3d4e5f60718", sg)
	require.NoError(t, err)

	require.NoError(t, registerDrainChecks())
	var out bytes.Buffer
	probe := featurelevel.NewProbe(nc, nodes)
	endpointDrain.connect(store, nodes, probe.Probe, nc, js, &out)
	t.Cleanup(func() {
		endpointDrain.mu.Lock()
		endpointDrain.store, endpointDrain.daemons, endpointDrain.proxy, endpointDrain.live, endpointDrain.rules = nil, nil, nil, nil, nil
		endpointDrain.mu.Unlock()
	})
	deps := featurelevel.RollbackDeps{
		Store: store, Probe: probe.Probe, Drains: featurelevel.DrainChecks, Build: featurelevel.Build,
		AckTimeout: 10 * time.Second, PollInterval: 50 * time.Millisecond,
	}

	_, err = featurelevel.RunRollback(t.Context(), 1, deps)
	var outstanding *featurelevel.DrainOutstandingError
	require.ErrorAs(t, err, &outstanding)
	assert.Equal(t, featurelevel.GatewayEndpoints, outstanding.Level)
	assert.Equal(t, 2, outstanding.Outstanding, "the endpoint and the prefix-list rule")
	level, _, err := store.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, level.Level, "a refused drain leaves the level where it was")

	require.NoError(t, endpoints.Delete(t.Context(), "000000000001.vpce-0a1b2c3d4e5f60718"))
	_, err = featurelevel.RunRollback(t.Context(), 1, deps)
	require.ErrorAs(t, err, &outstanding)
	assert.Equal(t, 1, outstanding.Outstanding, "the prefix-list rule still holds the rollback")

	require.NoError(t, groups.Delete(t.Context(), "000000000001.sg-0a1b2c3d4e5f60718"))
	got, err := featurelevel.RunRollback(t.Context(), 1, deps)
	require.NoError(t, err, "drain output: %s", out.String())
	assert.Equal(t, 1, got.Level)
}
