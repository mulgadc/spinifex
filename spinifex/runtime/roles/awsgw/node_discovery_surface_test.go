package awsgw_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	ec2awsapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// discoveryGateway serves signed EC2 requests with the node count from nodes.
func discoveryGateway(t *testing.T, nc *nats.Conn, nodes *awsgw.NodeDiscovery) http.Handler {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	gw := withEC2(withVerifier(gwfix.WithDeps(&pipeline.Config{DisableLogging: true, Region: gwfix.Region},
		gwfix.Deps{NATS: nc, Nodes: 1, IAM: &gwfix.PolicyIAM{
			IAM: gwfix.IAM{
				MasterKey: gwfix.MasterKey,
				AccessKeys: map[string]*iamdomain.AccessKey{
					gwfix.AccessKey: {
						AccessKeyID:     gwfix.AccessKey,
						SecretAccessKey: encryptedSecret,
						UserName:        "alice",
						AccountID:       authzAccountID,
						Status:          "Active",
					},
				},
			},
			GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return allowAllTaggingDocs(), nil },
		}})), func(d *ec2awsapi.Deps) { d.ActiveNodes = nodes.ActiveNodes })
	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)
	return r
}

// describeInstanceStatus sends one signed DescribeInstanceStatus, requires a
// 200 and returns the body.
func describeInstanceStatus(t *testing.T, r http.Handler) string {
	t.Helper()
	body := []byte("Action=DescribeInstanceStatus&Version=2016-11-15")
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	gwfix.SignRequestAs(t, req, body, gwfix.AccessKey, gwfix.SecretKey, "ec2", time.Now().UTC())

	resp := doRequest(r, req)
	defer resp.Body.Close()
	out := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, out)
	return out
}

// answerDescribeInstanceStatus answers DescribeInstanceStatus as node.
func answerDescribeInstanceStatus(t *testing.T, nc *nats.Conn, node, payload string) *nats.Subscription {
	t.Helper()
	sub, err := nc.Subscribe("ec2.DescribeInstanceStatus", func(msg *nats.Msg) {
		reply := nats.NewMsg(msg.Reply)
		reply.Header.Set(natsmsg.NodeIDHeader, node)
		reply.Data = []byte(payload)
		_ = msg.RespondMsg(reply)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())
	return sub
}

// countDiscoveries counts discovery publishes. When answer is false it never
// replies, so every gather finds no node.
func countDiscoveries(t *testing.T, nc *nats.Conn, answer bool) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	sub, err := nc.Subscribe(clusterv1.NodesDiscoverSubject, func(msg *nats.Msg) {
		n.Add(1)
		if answer {
			payload, _ := json.Marshal(clusterv1.NodeDiscoverResponse{Node: "node-1"})
			_ = msg.Respond(payload)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())
	return &n
}

func requireNoInlineDiscovery(t *testing.T, nc *nats.Conn, r http.Handler, discoveries *atomic.Int64, phase string) string {
	t.Helper()
	before := discoveries.Load()
	body := describeInstanceStatus(t, r)
	require.NoError(t, nc.Flush())
	if n := discoveries.Load() - before; n != 0 {
		t.Errorf("Q-367.1: %s DescribeInstanceStatus published %d node discovery request(s) on the request path, want 0 (node_discovery.go ActiveNodes gathers inline)", phase, n)
	}
	return body
}

// Verifies: Q-367.1. DescribeInstanceStatus must answer from the stored node
// count, cold and after the count is older than one refresh interval; a
// discovery gather on the request path adds its whole timeout to the call.
func TestQ367_DescribeInstanceStatusDoesNotRunDiscoveryInline(t *testing.T) {
	awsgw.SetActiveNodesTTLForTest(t, time.Hour)
	awsgw.SetDiscoverActiveNodesTimeoutForTest(t, 50*time.Millisecond)
	_, nc := testutil.StartTestNATS(t)
	discoveries := countDiscoveries(t, nc, true)
	answerAs(t, nc, "node-2", nil)
	answerDescribeInstanceStatus(t, nc, "node-1", `{"InstanceStatuses":[{"InstanceId":"i-0000000000000001"}]}`)
	answerDescribeInstanceStatus(t, nc, "node-2", `{"InstanceStatuses":[{"InstanceId":"i-0000000000000002"}]}`)

	nodes := &awsgw.NodeDiscovery{NATS: nc, Expected: 1}
	r := discoveryGateway(t, nc, nodes)

	requireNoInlineDiscovery(t, nc, r, discoveries, "cold")

	// One refresh stores the count; shrinking the interval after the loop stops
	// makes that count older than one interval without a wait.
	stop := runDiscovery(t, nodes)
	requireActiveNodes(t, nodes, 2)
	stop()
	require.Equal(t, int64(1), discoveries.Load(), "want exactly one refresh before the expired request")
	awsgw.SetActiveNodesTTLForTest(t, time.Nanosecond)
	body := requireNoInlineDiscovery(t, nc, r, discoveries, "expired")
	require.Contains(t, body, "i-0000000000000001")
	require.Contains(t, body, "i-0000000000000002", "the request did not use the stored count of 2")
}

// Verifies: Q-367.1. A request must not gather while the first background
// refresh is still running, nor after a refresh has failed.
func TestQ367_DescribeInstanceStatusDoesNotRunDiscoveryInlineWhileRefreshPendingOrFailed(t *testing.T) {
	setup := func(t *testing.T) (*nats.Conn, *atomic.Int64, *awsgw.NodeDiscovery, http.Handler) {
		t.Helper()
		_, nc := testutil.StartTestNATS(t)
		discoveries := countDiscoveries(t, nc, false)
		answerDescribeInstanceStatus(t, nc, "node-1", `{"InstanceStatuses":[]}`)
		nodes := &awsgw.NodeDiscovery{NATS: nc, Expected: 1}
		return nc, discoveries, nodes, discoveryGateway(t, nc, nodes)
	}

	t.Run("pending", func(t *testing.T) {
		awsgw.SetActiveNodesTTLForTest(t, time.Hour)
		awsgw.SetDiscoverActiveNodesTimeoutForTest(t, 2*time.Second)
		nc, discoveries, nodes, r := setup(t)

		runDiscovery(t, nodes)
		require.Eventually(t, func() bool { return discoveries.Load() == 1 }, 2*time.Second, 5*time.Millisecond)
		requireNoInlineDiscovery(t, nc, r, discoveries, "pending")
	})

	t.Run("failed", func(t *testing.T) {
		awsgw.SetActiveNodesTTLForTest(t, 50*time.Millisecond)
		awsgw.SetDiscoverActiveNodesTimeoutForTest(t, 10*time.Millisecond)
		nc, discoveries, nodes, r := setup(t)

		stop := runDiscovery(t, nodes)
		require.Eventually(t, func() bool { return discoveries.Load() >= 2 }, 2*time.Second, 5*time.Millisecond)
		stop()
		requireNoInlineDiscovery(t, nc, r, discoveries, "failed")
	})
}

// After a node leaves, a request waits the fan-out timeout until the next
// refresh, and answers at once after it.
func TestQ367_DescribeInstanceStatusAfterNodeLoss(t *testing.T) {
	awsgw.SetActiveNodesTTLForTest(t, time.Hour)
	awsgw.SetDiscoverActiveNodesTimeoutForTest(t, 50*time.Millisecond)
	_, nc := testutil.StartTestNATS(t)
	answerAs(t, nc, "node-1", nil)
	node2Discover := answerAs(t, nc, "node-2", nil)
	answerDescribeInstanceStatus(t, nc, "node-1", `{"InstanceStatuses":[{"InstanceId":"i-0000000000000001"}]}`)
	node2Status := answerDescribeInstanceStatus(t, nc, "node-2", `{"InstanceStatuses":[]}`)

	nodes := &awsgw.NodeDiscovery{NATS: nc, Expected: 3}
	r := discoveryGateway(t, nc, nodes)
	stop := runDiscovery(t, nodes)
	requireActiveNodes(t, nodes, 2)

	require.NoError(t, node2Discover.Unsubscribe())
	require.NoError(t, node2Status.Unsubscribe())
	require.NoError(t, nc.Flush())

	start := time.Now()
	body := describeInstanceStatus(t, r)
	if elapsed := time.Since(start); elapsed >= 4*time.Second {
		t.Errorf("before the refresh, DescribeInstanceStatus took %v, want under 4s (the 3s fan-out timeout bounds the wait)", elapsed)
	}
	require.Contains(t, body, "i-0000000000000001")

	stop()
	runDiscovery(t, nodes)
	requireActiveNodes(t, nodes, 1)

	start = time.Now()
	body = describeInstanceStatus(t, r)
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Errorf("after the refresh, DescribeInstanceStatus took %v, want under 500ms", elapsed)
	}
	require.Contains(t, body, "i-0000000000000001")
}
