package gateway_ec2_spotinstance

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_quota "github.com/mulgadc/spinifex/spinifex/handlers/quota"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// callOrder records the arrival order of named events from concurrent NATS
// callbacks and the quota KV, giving a single global sequence to assert on.
type callOrder struct {
	mu     sync.Mutex
	events []string
}

func (o *callOrder) record(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, name)
}

// indexOf returns the first position of name in the recorded sequence, or -1
// if it never happened.
func (o *callOrder) indexOf(name string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i, e := range o.events {
		if e == name {
			return i
		}
	}
	return -1
}

// recordingKV wraps a real jetstream.KeyValue and records every Get as a
// "quota-check" event. EnforceLaunch's only externally observable action is
// CheckVCPU's read of the counter, so instrumenting Get alone pins the moment
// the quota gate actually ran, using the real gate rather than a stand-in.
type recordingKV struct {
	jetstream.KeyValue

	order *callOrder
}

func (r *recordingKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	r.order.record("quota-check")
	return r.KeyValue.Get(ctx, key)
}

// TestRequestSpotInstances_QuotaGateRunsBeforeDispatchAndLaunch pins the
// ordering the vCPU quota gate depends on: EnforceLaunch must run before the
// capacity query (spinifex.node.status) and before the per-node VM launch
// (ec2.RunInstances.<type>.<node>), not merely "at some point" during the
// request. All three legs run for real: a real quota.Service backed by an
// embedded JetStream KV, and real NATS responders standing in for a capacity
// daemon and a launch daemon.
func TestRequestSpotInstances_QuotaGateRunsBeforeDispatchAndLaunch(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)

	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket:  handlers_quota.KVBucketAccountUsage,
		History: 1,
	})
	require.NoError(t, err)

	order := &callOrder{}
	quota := handlers_quota.New(handlers_quota.Limits{Enabled: true, VCPUs: 1000}, &recordingKV{KeyValue: kv, order: order})

	const instanceType = "t3.micro"
	const node = "node-1"

	// Stand-in capacity daemon: replies with one node that has room.
	statusSub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		order.record("capacity-dispatch")
		resp := types.NodeStatusResponse{
			Node:          node,
			InstanceTypes: []types.InstanceTypeCap{{Name: instanceType, Available: 5}},
		}
		data, _ := json.Marshal(resp)
		_ = nc.Publish(msg.Reply, data)
	})
	require.NoError(t, err)
	defer statusSub.Unsubscribe()

	// Stand-in launch daemon on the node the capacity daemon advertised.
	launchSub, err := nc.Subscribe(fmt.Sprintf("ec2.RunInstances.%s.%s", instanceType, node), func(msg *nats.Msg) {
		order.record("vm-launch")
		reservation := ec2.Reservation{
			ReservationId: aws.String("r-test"),
			Instances:     []*ec2.Instance{{InstanceId: aws.String("i-test"), InstanceType: aws.String(instanceType)}},
		}
		data, _ := json.Marshal(reservation)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer launchSub.Unsubscribe()

	// Stand-in SIR-persist daemon so the request completes cleanly.
	putSub, err := nc.Subscribe("ec2.PutSpotInstanceRequests", func(msg *nats.Msg) {
		_ = msg.Respond([]byte("{}"))
	})
	require.NoError(t, err)
	defer putSub.Unsubscribe()

	time.Sleep(50 * time.Millisecond) // let subscriptions propagate

	input := &ec2.RequestSpotInstancesInput{
		LaunchSpecification: &ec2.RequestSpotLaunchSpecification{
			ImageId:      aws.String("ami-0abcdef1234567890"),
			InstanceType: aws.String(instanceType),
		},
		InstanceCount: aws.Int64(1),
	}

	_, err = RequestSpotInstances(context.Background(), input, nc, nil, "test-account", "ap-southeast-2a", nil, quota, 1)
	require.NoError(t, err)

	quotaIdx := order.indexOf("quota-check")
	capacityIdx := order.indexOf("capacity-dispatch")
	launchIdx := order.indexOf("vm-launch")

	require.NotEqualf(t, -1, quotaIdx, "EnforceLaunch never touched the quota gate (events: %v)", order.events)
	require.NotEqualf(t, -1, capacityIdx, "capacity dispatch never ran (events: %v)", order.events)
	require.NotEqualf(t, -1, launchIdx, "VM launch never ran (events: %v)", order.events)

	assert.Less(t, quotaIdx, capacityIdx, "quota gate must run before capacity dispatch")
	assert.Less(t, quotaIdx, launchIdx, "quota gate must run before VM launch")
}
