package gateway_ec2_vpc

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := &server.Options{
		Host:   "127.0.0.1",
		Port:   -1,
		NoLog:  true,
		NoSigs: true,
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })
	return nc
}

// describeNetworkInterfacesResponder seeds a DescribeNetworkInterfaces NATS
// responder that returns the supplied attachment-id → instance-id mapping.
func describeNetworkInterfacesResponder(t *testing.T, nc *nats.Conn, attachID, instanceID, eniID string) {
	t.Helper()
	sub, err := nc.Subscribe("ec2.DescribeNetworkInterfaces", func(msg *nats.Msg) {
		resp := ec2.DescribeNetworkInterfacesOutput{
			NetworkInterfaces: []*ec2.NetworkInterface{
				{
					NetworkInterfaceId: aws.String(eniID),
					Attachment: &ec2.NetworkInterfaceAttachment{
						AttachmentId: aws.String(attachID),
						InstanceId:   aws.String(instanceID),
					},
				},
			},
		}
		data, _ := json.Marshal(resp)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// --- AttachNetworkInterface ---

func TestAttachNetworkInterface_NilInput(t *testing.T) {
	_, err := AttachNetworkInterface(nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestAttachNetworkInterface_MissingNetworkInterfaceID(t *testing.T) {
	_, err := AttachNetworkInterface(&ec2.AttachNetworkInterfaceInput{
		InstanceId:  aws.String("i-abc"),
		DeviceIndex: aws.Int64(0),
	}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestAttachNetworkInterface_EmptyNetworkInterfaceID(t *testing.T) {
	_, err := AttachNetworkInterface(&ec2.AttachNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(""),
		InstanceId:         aws.String("i-abc"),
		DeviceIndex:        aws.Int64(0),
	}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestAttachNetworkInterface_MissingInstanceID(t *testing.T) {
	_, err := AttachNetworkInterface(&ec2.AttachNetworkInterfaceInput{
		NetworkInterfaceId: aws.String("eni-1"),
		DeviceIndex:        aws.Int64(0),
	}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestAttachNetworkInterface_MissingDeviceIndex(t *testing.T) {
	_, err := AttachNetworkInterface(&ec2.AttachNetworkInterfaceInput{
		NetworkInterfaceId: aws.String("eni-1"),
		InstanceId:         aws.String("i-abc"),
	}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestAttachNetworkInterface_NoResponders(t *testing.T) {
	nc := newTestNATS(t)
	_, err := AttachNetworkInterface(&ec2.AttachNetworkInterfaceInput{
		NetworkInterfaceId: aws.String("eni-1"),
		InstanceId:         aws.String("i-no-responder"),
		DeviceIndex:        aws.Int64(0),
	}, nc, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidInstanceIDNotFound)
}

func TestAttachNetworkInterface_Success(t *testing.T) {
	nc := newTestNATS(t)

	var received types.EC2InstanceCommand
	var receivedAccount string
	sub, err := nc.Subscribe("ec2.cmd.i-success", func(msg *nats.Msg) {
		receivedAccount = msg.Header.Get(utils.AccountIDHeader)
		_ = json.Unmarshal(msg.Data, &received)
		data, _ := json.Marshal(ec2.AttachNetworkInterfaceOutput{
			AttachmentId: aws.String("eni-attach-xyz"),
		})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	out, err := AttachNetworkInterface(&ec2.AttachNetworkInterfaceInput{
		NetworkInterfaceId: aws.String("eni-feedface"),
		InstanceId:         aws.String("i-success"),
		DeviceIndex:        aws.Int64(2),
	}, nc, testAccountID)

	require.NoError(t, err)
	require.NotNil(t, out.AttachmentId)
	assert.Equal(t, "eni-attach-xyz", *out.AttachmentId)
	assert.Equal(t, testAccountID, receivedAccount)
	assert.True(t, received.Attributes.AttachENI)
	require.NotNil(t, received.AttachENIData)
	assert.Equal(t, "eni-feedface", received.AttachENIData.NetworkInterfaceID)
	assert.Equal(t, int64(2), received.AttachENIData.DeviceIndex)
	assert.Equal(t, "i-success", received.ID)
}

func TestAttachNetworkInterface_DaemonErrorResponse(t *testing.T) {
	nc := newTestNATS(t)

	sub, err := nc.Subscribe("ec2.cmd.i-err", func(msg *nats.Msg) {
		_ = msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorIncorrectInstanceState))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	_, err = AttachNetworkInterface(&ec2.AttachNetworkInterfaceInput{
		NetworkInterfaceId: aws.String("eni-bad"),
		InstanceId:         aws.String("i-err"),
		DeviceIndex:        aws.Int64(0),
	}, nc, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorIncorrectInstanceState)
}

func TestAttachNetworkInterface_MalformedDaemonResponse(t *testing.T) {
	nc := newTestNATS(t)

	sub, err := nc.Subscribe("ec2.cmd.i-malformed", func(msg *nats.Msg) {
		_ = msg.Respond([]byte("not-json"))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	_, err = AttachNetworkInterface(&ec2.AttachNetworkInterfaceInput{
		NetworkInterfaceId: aws.String("eni-bad"),
		InstanceId:         aws.String("i-malformed"),
		DeviceIndex:        aws.Int64(0),
	}, nc, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorServerInternal)
}

// --- DetachNetworkInterface ---

func TestDetachNetworkInterface_NilInput(t *testing.T) {
	_, err := DetachNetworkInterface(nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDetachNetworkInterface_MissingAttachmentID(t *testing.T) {
	_, err := DetachNetworkInterface(&ec2.DetachNetworkInterfaceInput{}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDetachNetworkInterface_EmptyAttachmentID(t *testing.T) {
	_, err := DetachNetworkInterface(&ec2.DetachNetworkInterfaceInput{
		AttachmentId: aws.String(""),
	}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDetachNetworkInterface_UnknownAttachment(t *testing.T) {
	nc := newTestNATS(t)

	sub, err := nc.Subscribe("ec2.DescribeNetworkInterfaces", func(msg *nats.Msg) {
		data, _ := json.Marshal(ec2.DescribeNetworkInterfacesOutput{})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	_, err = DetachNetworkInterface(&ec2.DetachNetworkInterfaceInput{
		AttachmentId: aws.String("eni-attach-missing"),
	}, nc, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidAttachmentIDNotFound)
}

func TestDetachNetworkInterface_DescribeErrorResponse(t *testing.T) {
	nc := newTestNATS(t)

	sub, err := nc.Subscribe("ec2.DescribeNetworkInterfaces", func(msg *nats.Msg) {
		_ = msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorServerInternal))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	_, err = DetachNetworkInterface(&ec2.DetachNetworkInterfaceInput{
		AttachmentId: aws.String("eni-attach-1"),
	}, nc, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidAttachmentIDNotFound)
}

func TestDetachNetworkInterface_AttachmentWithoutInstance(t *testing.T) {
	nc := newTestNATS(t)

	sub, err := nc.Subscribe("ec2.DescribeNetworkInterfaces", func(msg *nats.Msg) {
		resp := ec2.DescribeNetworkInterfacesOutput{
			NetworkInterfaces: []*ec2.NetworkInterface{
				{
					NetworkInterfaceId: aws.String("eni-orphan"),
					Attachment: &ec2.NetworkInterfaceAttachment{
						AttachmentId: aws.String("eni-attach-orphan"),
					},
				},
			},
		}
		data, _ := json.Marshal(resp)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	_, err = DetachNetworkInterface(&ec2.DetachNetworkInterfaceInput{
		AttachmentId: aws.String("eni-attach-orphan"),
	}, nc, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidAttachmentIDNotFound)
}

func TestDetachNetworkInterface_NoResponders(t *testing.T) {
	nc := newTestNATS(t)
	describeNetworkInterfacesResponder(t, nc, "eni-attach-1", "i-no-daemon", "eni-1")

	_, err := DetachNetworkInterface(&ec2.DetachNetworkInterfaceInput{
		AttachmentId: aws.String("eni-attach-1"),
	}, nc, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidInstanceIDNotFound)
}

func TestDetachNetworkInterface_Success(t *testing.T) {
	nc := newTestNATS(t)
	describeNetworkInterfacesResponder(t, nc, "eni-attach-ok", "i-detach", "eni-detachable")

	var received types.EC2InstanceCommand
	var receivedAccount string
	sub, err := nc.Subscribe("ec2.cmd.i-detach", func(msg *nats.Msg) {
		receivedAccount = msg.Header.Get(utils.AccountIDHeader)
		_ = json.Unmarshal(msg.Data, &received)
		data, _ := json.Marshal(ec2.DetachNetworkInterfaceOutput{})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	_, err = DetachNetworkInterface(&ec2.DetachNetworkInterfaceInput{
		AttachmentId: aws.String("eni-attach-ok"),
		Force:        aws.Bool(true),
	}, nc, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, testAccountID, receivedAccount)
	assert.True(t, received.Attributes.DetachENI)
	require.NotNil(t, received.DetachENIData)
	assert.Equal(t, "eni-attach-ok", received.DetachENIData.AttachmentID)
	assert.True(t, received.DetachENIData.Force)
	assert.Equal(t, "i-detach", received.ID)
}

func TestDetachNetworkInterface_DaemonErrorResponse(t *testing.T) {
	nc := newTestNATS(t)
	describeNetworkInterfacesResponder(t, nc, "eni-attach-err", "i-err", "eni-x")

	sub, err := nc.Subscribe("ec2.cmd.i-err", func(msg *nats.Msg) {
		_ = msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorIncorrectInstanceState))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	_, err = DetachNetworkInterface(&ec2.DetachNetworkInterfaceInput{
		AttachmentId: aws.String("eni-attach-err"),
	}, nc, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorIncorrectInstanceState)
}

func TestDetachNetworkInterface_MalformedDaemonResponse(t *testing.T) {
	nc := newTestNATS(t)
	describeNetworkInterfacesResponder(t, nc, "eni-attach-bad", "i-bad", "eni-bad")

	sub, err := nc.Subscribe("ec2.cmd.i-bad", func(msg *nats.Msg) {
		_ = msg.Respond([]byte("not-json"))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	_, err = DetachNetworkInterface(&ec2.DetachNetworkInterfaceInput{
		AttachmentId: aws.String("eni-attach-bad"),
	}, nc, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorServerInternal)
}
