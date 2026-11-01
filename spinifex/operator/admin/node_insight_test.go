package admin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetNodeMetricsAsksThePolicyBeforeAnythingElse(t *testing.T) {
	var asked []string
	deny := func(_, act string, _ []string, _ iampolicy.ConditionKeys) error {
		asked = append(asked, act)
		return errors.New(awserrors.ErrorAccessDenied)
	}
	_, err := serve(t, Deps{}, awsidentifiers.DefaultAccountID(), action(GetNodeMetricsAction, "NodeId", "node1"), deny)
	requireCode(t, err, awserrors.ErrorAccessDenied)
	assert.Equal(t, []string{GetNodeMetricsAction}, asked)
}

func TestGetNodeMetricsRefusesANodeOutsideTheCluster(t *testing.T) {
	d := Deps{NodeIDs: []string{"node1", "node2"}}
	for _, node := range []string{"node9", "node1.>", "*"} {
		_, err := serve(t, d, awsidentifiers.DefaultAccountID(), action(GetNodeMetricsAction, "NodeId", node), nil)
		code, _ := awserrors.ResolveErrorCode(err)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, code, node)
	}
}

func TestNetworkTestPairs(t *testing.T) {
	nodes := []string{"c", "a", "b"}

	pair, err := networkTestPairs(nodes, "a", "b")
	require.NoError(t, err)
	assert.Equal(t, [][2]string{{"a", "b"}, {"b", "a"}}, directions(pair))

	mesh, err := networkTestPairs(nodes, "b", "")
	require.NoError(t, err)
	assert.Equal(t, [][2]string{{"b", "a"}, {"b", "c"}, {"a", "b"}, {"a", "c"}, {"c", "b"}, {"c", "a"}}, directions(mesh))

	for _, c := range []struct{ source, target, code string }{
		{"a", "a", awserrors.ErrorInvalidParameterCombination},
		{"", "b", awserrors.ErrorMissingParameter},
		{"z", "", awserrors.ErrorInvalidParameterValue},
		{"a", "z", awserrors.ErrorInvalidParameterValue},
	} {
		_, err := networkTestPairs(nodes, c.source, c.target)
		code, _ := awserrors.ResolveErrorCode(err)
		assert.Equal(t, c.code, code, "%s -> %s", c.source, c.target)
	}

	_, err = networkTestPairs([]string{"solo"}, "", "")
	assert.Error(t, err, "one node has nobody to test against")
}

func directions(runs []NetworkTestResult) [][2]string {
	out := make([][2]string, len(runs))
	for i, r := range runs {
		out[i] = [2]string{r.From, r.To}
	}
	return out
}

func TestStartNetworkTestValidatesBeforeClaiming(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	d := Deps{NATS: nc, NetworkTests: NewKVNetworkTests(js), NodeIDs: []string{"a", "b"}}
	for _, args := range []map[string]string{
		{"Plane": "mgmt"},
		{"Plane": "lan", "DurationSeconds": "600"},
		{"Plane": "lan", "Streams": "0"},
		{"Plane": "lan", "Source": "a", "Target": "a"},
	} {
		_, err := d.StartNetworkTest(context.Background(), args)
		assert.Error(t, err, "%v", args)
	}
	tests, err := d.NetworkTests.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, tests, "a refused request records nothing")
}

func TestNetworkTestClaimIsClusterWide(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	first, second := NewKVNetworkTests(js), NewKVNetworkTests(js)

	require.NoError(t, first.Claim(ctx, "nt-1", time.Now().Add(time.Minute)))
	assert.ErrorIs(t, second.Claim(ctx, "nt-2", time.Now().Add(time.Minute)), ErrNetworkTestBusy)

	require.NoError(t, second.Release(ctx, "nt-2"), "releasing a claim it does not hold is a no-op")
	assert.ErrorIs(t, second.Claim(ctx, "nt-2", time.Now().Add(time.Minute)), ErrNetworkTestBusy)

	require.NoError(t, first.Release(ctx, "nt-1"))
	require.NoError(t, second.Claim(ctx, "nt-2", time.Now().Add(-time.Second)))
	require.NoError(t, first.Claim(ctx, "nt-3", time.Now().Add(time.Minute)), "an expired claim is taken over")
}

// fakeNode answers the serve and run halves as a daemon would, reporting a
// fixed throughput, or failing the serve when broken.
func fakeNode(t *testing.T, nc *nats.Conn, node, addr string, bps float64, broken bool) {
	t.Helper()
	serveSub, err := nc.Subscribe(clusterv1.NetTestServeSubject(node), func(m *nats.Msg) {
		resp := clusterv1.NetTestServeResponse{Address: addr, Port: clusterv1.NetTestPort}
		if broken {
			resp = clusterv1.NetTestServeResponse{Error: "iperf (version 2) is not installed on this node"}
		}
		data, _ := json.Marshal(resp)
		_ = m.Respond(data)
	})
	require.NoError(t, err)
	runSub, err := nc.Subscribe(clusterv1.NetTestRunSubject(node), func(m *nats.Msg) {
		var req clusterv1.NetTestRunRequest
		_ = json.Unmarshal(m.Data, &req)
		data, _ := json.Marshal(clusterv1.NetTestRunResponse{
			SourceAddress: addr, BitsPerSecond: bps, Streams: req.Streams, LinkMbps: 25000, Tool: "iperf2",
		})
		_ = m.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = serveSub.Unsubscribe(); _ = runSub.Unsubscribe() })
}

func TestNetworkTestRunsEveryDirectionAndRecordsEachResult(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	d := Deps{NATS: nc, NetworkTests: NewKVNetworkTests(js), NodeIDs: []string{"a", "b", "c"}}
	fakeNode(t, nc, "a", "10.10.8.4", 23e9, false)
	fakeNode(t, nc, "b", "10.10.8.5", 22e9, false)
	fakeNode(t, nc, "c", "10.10.8.6", 0, true)

	started, err := d.StartNetworkTest(ctx, map[string]string{"Plane": "lan", "DurationSeconds": "5", "Streams": "2"})
	require.NoError(t, err)
	assert.Equal(t, NetworkTestRunning, started.Status)
	assert.Len(t, started.Runs, 6)

	_, err = d.StartNetworkTest(ctx, map[string]string{"Plane": "lan"})
	code, _ := awserrors.ResolveErrorCode(err)
	assert.Equal(t, awserrors.ErrorIncorrectState, code, "one test at a time across the cluster")

	var done NetworkTest
	require.Eventually(t, func() bool {
		out, err := d.DescribeNetworkTests(ctx, map[string]string{"TestId": started.TestID})
		if err != nil || len(out.Tests) != 1 {
			return false
		}
		done = out.Tests[0]
		return done.Status != NetworkTestRunning
	}, 10*time.Second, 20*time.Millisecond)

	assert.Equal(t, NetworkTestComplete, done.Status, "some runs succeeded")
	byDirection := map[[2]string]NetworkTestResult{}
	for _, r := range done.Runs {
		byDirection[[2]string{r.From, r.To}] = r
	}
	ab := byDirection[[2]string{"a", "b"}]
	assert.Equal(t, NetworkTestComplete, ab.Status)
	assert.InDelta(t, 23e9, ab.BitsPerSecond, 1)
	assert.Equal(t, 2, ab.Streams)
	assert.Equal(t, NetworkTestFailed, byDirection[[2]string{"a", "c"}].Status)
	assert.Contains(t, byDirection[[2]string{"a", "c"}].Error, "not installed")
	assert.Equal(t, NetworkTestComplete, byDirection[[2]string{"c", "a"}].Status, "c can still send")

	again, err := d.StartNetworkTest(ctx, map[string]string{"Plane": "lan", "Source": "a", "Target": "b", "DurationSeconds": "5"})
	require.NoError(t, err, "the claim is released when the test ends")

	list, err := d.DescribeNetworkTests(ctx, map[string]string{})
	require.NoError(t, err)
	require.Len(t, list.Tests, 2)
	assert.Equal(t, again.TestID, list.Tests[0].TestID, "newest first")

	require.Eventually(t, func() bool {
		out, err := d.DescribeNetworkTests(ctx, map[string]string{"TestId": again.TestID})
		return err == nil && out.Tests[0].Status != NetworkTestRunning
	}, 10*time.Second, 20*time.Millisecond)
}

func TestDescribeNetworkTestsUnknownID(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	d := Deps{NetworkTests: NewKVNetworkTests(js)}
	_, err := d.DescribeNetworkTests(context.Background(), map[string]string{"TestId": "nt-missing"})
	code, _ := awserrors.ResolveErrorCode(err)
	assert.Equal(t, awserrors.ErrorResourceNotFound, code)
}
