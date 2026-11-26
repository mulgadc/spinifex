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

// Verifies: Q-367.1. DescribeInstanceStatus must answer from the last known
// node count, cold and after the count ages past its TTL; a discovery gather
// on the request path adds its whole timeout to the call.
func TestQ367_DescribeInstanceStatusDoesNotRunDiscoveryInline(t *testing.T) {
	awsgw.SetDiscoverActiveNodesTimeoutForTest(t, 50*time.Millisecond)
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	_, nc := testutil.StartTestNATS(t)

	var discoveries atomic.Int64
	discoverSub, err := nc.Subscribe(clusterv1.NodesDiscoverSubject, func(msg *nats.Msg) {
		discoveries.Add(1)
		payload, _ := json.Marshal(clusterv1.NodeDiscoverResponse{Node: "node-1"})
		_ = msg.Respond(payload)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = discoverSub.Unsubscribe() })
	daemonSub, err := nc.Subscribe("ec2.DescribeInstanceStatus", func(msg *nats.Msg) {
		reply := nats.NewMsg(msg.Reply)
		reply.Header.Set(natsmsg.NodeIDHeader, "node-1")
		reply.Data = []byte(`{"InstanceStatuses":[]}`)
		_ = msg.RespondMsg(reply)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = daemonSub.Unsubscribe() })
	require.NoError(t, nc.Flush())

	nodes := &awsgw.NodeDiscovery{NATS: nc, Expected: 1}
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

	describe := func(phase string) {
		t.Helper()
		before := discoveries.Load()
		body := []byte("Action=DescribeInstanceStatus&Version=2016-11-15")
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		req.Host = "localhost:9999"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		gwfix.SignRequestAs(t, req, body, gwfix.AccessKey, gwfix.SecretKey, "ec2", time.Now().UTC())

		resp := doRequest(r, req)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, readBody(t, resp))
		require.NoError(t, nc.Flush())
		if n := discoveries.Load() - before; n != 0 {
			t.Errorf("Q-367.1: %s DescribeInstanceStatus published %d node discovery request(s) on the request path, want 0 (node_discovery.go ActiveNodes gathers inline)", phase, n)
		}
	}

	describe("cold")
	awsgw.AgeActiveNodesForTest(nodes)
	describe("expired")
}
