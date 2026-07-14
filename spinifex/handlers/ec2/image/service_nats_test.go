package handlers_ec2_image

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNATSCreateImage_OwnerServerErrorNotMaskedByNotFound ensures the owner's
// 5xx fault (e.g. no root volume / snapshot failure) is surfaced rather than
// masked by a non-owner's NotFound, so the client retries instead of seeing a
// misleading "instance not found".
func TestNATSCreateImage_OwnerServerErrorNotMaskedByNotFound(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	_, err := nc.Subscribe("ec2.CreateImage", func(msg *nats.Msg) {
		_ = msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorServerInternal))
	})
	require.NoError(t, err)

	nc2, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	defer nc2.Close()
	_, err = nc2.Subscribe("ec2.CreateImage", func(msg *nats.Msg) {
		_ = msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorInvalidInstanceIDNotFound))
	})
	require.NoError(t, err)

	nc.Flush()
	nc2.Flush()

	svc := NewNATSImageService(nc, 2)
	_, err = svc.CreateImage(context.Background(), &ec2.CreateImageInput{
		InstanceId: aws.String("i-abc123"),
		Name:       aws.String("my-image"),
	}, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// TestNATSCreateImage_AllNotFound returns NotFound when no node owns the instance.
func TestNATSCreateImage_AllNotFound(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	_, err := nc.Subscribe("ec2.CreateImage", func(msg *nats.Msg) {
		_ = msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorInvalidInstanceIDNotFound))
	})
	require.NoError(t, err)
	nc.Flush()

	svc := NewNATSImageService(nc, 1)
	_, err = svc.CreateImage(context.Background(), &ec2.CreateImageInput{
		InstanceId: aws.String("i-missing"),
		Name:       aws.String("my-image"),
	}, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}
