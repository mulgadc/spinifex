package pipeline

import (
	"context"
	"encoding/json"
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
}

func (s *amiGetFailingStore) GetObject(ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if aws.StringValue(input.Key) == s.failKey {
		return nil, awserr.NewRequestFailure(
			awserr.New("InternalError", "We encountered an internal error. Please try again.", nil), 500, "req-1")
	}
	return s.MemoryObjectStore.GetObject(ctx, input)
}

// serveNodeLaunch puts a real instance service on node-1 behind the gateway.
// It answers the launch with the node's prepare error, encoded as the daemon's
// launch handler encodes it.
func serveNodeLaunch(t *testing.T, app ec2PipelineApp, nc *nats.Conn, store objectstore.ObjectStore) {
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

	app.Daemon.handle("ec2.RunInstances.t3.micro.node-1", func(m *nats.Msg) []byte {
		var input ec2.RunInstancesInput
		if err := json.Unmarshal(m.Data, &input); err != nil {
			return awserrors.GenerateErrorPayload(awserrors.ErrorServerInternal)
		}
		_, _, _, err := node.PrepareRunInstances(context.Background(), &input, authzAccountID, "")
		if err == nil {
			t.Errorf("the node prepared a launch with no launchable AMI")
			return awserrors.GenerateErrorPayload(awserrors.ErrorServerInternal)
		}
		_, message, _ := awserrors.ResolveErrorDetail(err)
		return awserrors.GenerateErrorPayloadWithMessage(awserrors.ValidErrorCodeFromError(err), message)
	})
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
			if tc.failRead {
				key, err := ebsmetadata.AMIKey(amiReadTestImage)
				require.NoError(t, err)
				store = &amiGetFailingStore{MemoryObjectStore: memory, failKey: key}
			}

			app := ec2Pipeline(t, nil, allowEC2())
			serveNodeLaunch(t, app, app.NC, store)

			got := probeEC2(t, app.Handler, runInstancesBase+"&ImageId="+amiReadTestImage)

			assert.Equal(t, tc.wantStatus, got.Status, got.Body)
			assert.Equal(t, tc.wantCode, got.Code, got.Body)
			assert.Equal(t, 1, app.Daemon.count("ec2.RunInstances.t3.micro.node-1"), "the launch must reach the node")
		})
	}
}
