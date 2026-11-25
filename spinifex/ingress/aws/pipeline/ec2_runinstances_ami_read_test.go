package pipeline

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	ec2image "github.com/mulgadc/spinifex/spinifex/domains/ec2/image"
	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	amiReadTestImage  = "ami-0123456789abcdef0"
	amiReadTestBucket = "spinifex-test"
)

// amiGetFailingStore answers each GET of failKey with the S3 client's error for
// a predastore 500, as the blob layer gave when it was down.
type amiGetFailingStore struct {
	*objectstore.MemoryObjectStore

	failKey string
	failed  atomic.Int32
}

func (s *amiGetFailingStore) GetObject(ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if aws.StringValue(input.Key) == s.failKey {
		s.failed.Add(1)
		return nil, awserr.NewRequestFailure(
			awserr.New("InternalError", "We encountered an internal error. Please try again.", nil), 500, "req-1")
	}
	return s.MemoryObjectStore.GetObject(ctx, input)
}

// serveNodeLaunch puts a real instance service on node-1 behind the gateway.
// It answers the launch with the node's prepare error, encoded as the daemon's
// launch handler encodes it, and sends that error's code on the returned channel.
func serveNodeLaunch(t *testing.T, app ec2PipelineApp, nc *nats.Conn, store objectstore.ObjectStore) <-chan string {
	t.Helper()
	status, err := json.Marshal(clusterv1.NodeStatusResponse{
		Node:          "node-1",
		InstanceTypes: []clusterv1.InstanceTypeCap{{Name: "t3.micro", Available: 1}},
	})
	require.NoError(t, err)
	sub, err := nc.Subscribe(clusterv1.NodeStatusSubject, func(m *nats.Msg) {
		out := nats.NewMsg(m.Reply)
		out.Data = status
		out.Header.Set(natsmsg.NodeIDHeader, "node-1")
		_ = m.RespondMsg(out)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())

	types := map[string]*ec2.InstanceTypeInfo{"t3.micro": {InstanceType: aws.String("t3.micro")}}
	node := ec2instance.NewInstanceServiceImpl(nil, types, nil, nil, nil, nil, nil)
	node.SetRunInstancesDeps(ec2image.NewImageServiceImplWithStore(store, amiReadTestBucket), nil, nil, nil)

	prepared := make(chan string, 1)
	app.Daemon.handle("ec2.RunInstances.t3.micro.node-1", func(m *nats.Msg) []byte {
		var input ec2.RunInstancesInput
		if err := json.Unmarshal(m.Data, &input); err != nil {
			t.Errorf("the node could not decode the launch: %v", err)
			return awserrors.GenerateErrorPayload(awserrors.ErrorServerInternal)
		}
		_, _, _, err := node.PrepareRunInstances(context.Background(), &input, authzAccountID, "")
		if err == nil {
			t.Errorf("the node prepared a launch with no launchable AMI")
			return awserrors.GenerateErrorPayload(awserrors.ErrorServerInternal)
		}
		code, message, ok := awserrors.ResolveErrorDetail(err)
		if !ok {
			code = "unresolved: " + err.Error()
		}
		prepared <- code
		return awserrors.GenerateErrorPayloadWithMessage(awserrors.ValidErrorCodeFromError(err), message)
	})
	return prepared
}

// RunInstances answers not-found only for an AMI that does not exist. When the
// node cannot read an existing AMI, the client gets a 5xx, which SDKs retry.
func TestEC2Pipeline_RunInstancesAMIReadFailure(t *testing.T) {
	cases := []struct {
		name       string
		registered bool
		failRead   bool
		wantStatus int
		wantCode   string
	}{
		{name: "absent AMI", wantStatus: 400, wantCode: awserrors.ErrorInvalidAMIIDNotFound},
		{name: "AMI read fails", registered: true, failRead: true, wantStatus: 500, wantCode: awserrors.ErrorServerInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			memory := objectstore.NewMemoryObjectStore()
			if tc.registered {
				require.NoError(t, ebsmetadata.NewStore(memory, amiReadTestBucket).PutAMI(context.Background(), ebsmetadata.AMI{
					ImageID:         amiReadTestImage,
					ImageOwnerAlias: "spinifex",
					SnapshotID:      "snap-" + amiReadTestImage,
					State:           ebsmetadata.AMIStateAvailable,
				}))
			}
			var store objectstore.ObjectStore = memory
			failing := &amiGetFailingStore{MemoryObjectStore: memory}
			if tc.failRead {
				key, err := ebsmetadata.AMIKey(amiReadTestImage)
				require.NoError(t, err)
				failing.failKey = key
				store = failing
			}

			app := ec2Pipeline(t, nil, allowEC2())
			prepared := serveNodeLaunch(t, app, app.NC, store)

			got := probeEC2(t, app.Handler, runInstancesBase+"&ImageId="+amiReadTestImage)

			assert.Equal(t, tc.wantStatus, got.Status, got.Body)
			assert.Equal(t, tc.wantCode, got.Code, got.Body)
			select {
			case code := <-prepared:
				assert.Equal(t, tc.wantCode, code, "the node's own answer")
			default:
				t.Fatal("the launch did not reach the node's prepare step")
			}
			if tc.failRead {
				assert.Positive(t, failing.failed.Load(), "the node must read the AMI and get the 500")
			}
		})
	}
}

// An ID that can name no AMI document is malformed even when no node has room,
// so the answer does not depend on placement. A launch template that supplies
// the ID is refused the same way, before any capacity query.
func TestEC2Pipeline_RunInstancesUnkeyableAMIIDWithNoCapacity(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		template bool
	}{
		{name: "request ImageId", body: runInstancesBase + "&ImageId=ami-a%2Fb"},
		{name: "launch template ImageId", body: runInstancesBase + "&LaunchTemplate.LaunchTemplateName=lt", template: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := ec2Pipeline(t, nil, allowEC2())
			if tc.template {
				app.Daemon.reply("ec2.DescribeLaunchTemplateVersions",
					`{"LaunchTemplateVersions":[{"LaunchTemplateData":{"ImageId":"ami-a/b"}}]}`)
			}
			var capacityQueries atomic.Int32
			sub, err := app.NC.Subscribe(clusterv1.NodeStatusSubject, func(*nats.Msg) { capacityQueries.Add(1) })
			require.NoError(t, err)
			t.Cleanup(func() { _ = sub.Unsubscribe() })
			require.NoError(t, app.NC.Flush())

			got := probeEC2(t, app.Handler, tc.body)

			assert.Equal(t, 400, got.Status, got.Body)
			assert.Equal(t, awserrors.ErrorInvalidAMIIDMalformed, got.Code, got.Body)
			require.NoError(t, app.NC.Flush())
			assert.Zero(t, capacityQueries.Load(), "validation must refuse the ID before placement")
			if tc.template {
				assert.Equal(t, 1, app.Daemon.count("ec2.DescribeLaunchTemplateVersions"))
			}
		})
	}
}
