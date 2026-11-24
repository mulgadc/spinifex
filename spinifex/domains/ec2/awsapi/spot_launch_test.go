//test:in-package — drives RunInstances through Dispatch to reach the spot branch.

package awsapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	types "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	ec2spotinstance "github.com/mulgadc/spinifex/spinifex/domains/ec2/spotinstance"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RunInstances with MarketType=spot must launch a spot instance backed by a
// persisted one-time spot request, and return the spot lineage on the instance.
func TestRunInstances_SpotMarketCreatesSpotRequest(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	const instanceType, node = "t3.micro", "node-1"

	subscribe := func(subject string, cb nats.MsgHandler) {
		sub, err := nc.Subscribe(subject, cb)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	subscribe("spinifex.node.status", func(m *nats.Msg) {
		data, _ := json.Marshal(types.NodeStatusResponse{
			Node:          node,
			InstanceTypes: []types.InstanceTypeCap{{Name: instanceType, Available: 5}},
		})
		_ = m.Respond(data)
	})
	subscribe("ec2.RunInstances."+instanceType+"."+node, func(m *nats.Msg) {
		data, _ := json.Marshal(ec2.Reservation{
			ReservationId: aws.String("r-test"),
			Instances:     []*ec2.Instance{{InstanceId: aws.String("i-spot"), InstanceType: aws.String(instanceType)}},
		})
		_ = m.Respond(data)
	})
	persisted := make(chan ec2spotinstance.PutSpotRequestsInput, 1)
	subscribe("ec2.PutSpotInstanceRequests", func(m *nats.Msg) {
		var in ec2spotinstance.PutSpotRequestsInput
		_ = json.Unmarshal(m.Data, &in)
		persisted <- in
		_ = m.Respond([]byte("{}"))
	})
	subscribe("ec2.cmd.>", func(m *nats.Msg) { _ = m.Respond([]byte("{}")) })

	h := NewHandler(Deps{
		NATS: nc, IAM: noProfiles{}, Quota: allowQuota{},
		ActiveNodes: func(context.Context) int { return 1 }, ExpectedNodes: 1,
		NodeIDs: []string{node}, AZ: "ap-southeast-2a",
	})

	w := httptest.NewRecorder()
	err := h.Dispatch(w, dispatch.Invocation{
		Request: formRequest("Action=RunInstances&ImageId=ami-0abcdef1234567890&InstanceType=" + instanceType +
			"&MinCount=1&MaxCount=1&InstanceMarketOptions.MarketType=spot&InstanceMarketOptions.SpotOptions.MaxPrice=0.05"),
		AccountID: testAccountID, Service: ServiceName, Region: testRegion, Authorize: allowAll,
	})
	require.NoError(t, err)

	var put ec2spotinstance.PutSpotRequestsInput
	select {
	case put = <-persisted:
	case <-time.After(5 * time.Second):
		t.Fatal("RunInstances with MarketType=spot persisted no spot instance request")
	}
	require.Len(t, put.Requests, 1)
	sir := put.Requests[0]
	assert.Equal(t, "i-spot", aws.StringValue(sir.InstanceId))
	assert.Equal(t, ec2.SpotInstanceTypeOneTime, aws.StringValue(sir.Type))
	assert.Equal(t, ec2.SpotInstanceStateActive, aws.StringValue(sir.State))
	assert.Equal(t, "0.05", aws.StringValue(sir.SpotPrice))

	fields := responseFields(t, w.Body.Bytes())
	assert.Contains(t, fields, "RunInstancesResponse/instancesSet/item/instanceLifecycle=spot")
	assert.Contains(t, fields, "RunInstancesResponse/instancesSet/item/spotInstanceRequestId="+aws.StringValue(sir.SpotInstanceRequestId))
}
