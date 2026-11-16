package volume

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescribeVolumeAttribute_AutoEnableIO(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	const id = "vol-0123456789abcdef0"
	sub, err := nc.Subscribe("ec2.DescribeVolumes", func(m *nats.Msg) {
		var in ec2.DescribeVolumesInput
		if json.Unmarshal(m.Data, &in) != nil || len(in.VolumeIds) != 1 || *in.VolumeIds[0] != id {
			_ = m.Respond(awserrors.GenerateErrorPayload(awserrors.ErrorInvalidVolumeNotFound))
			return
		}
		out, _ := json.Marshal(ec2.DescribeVolumesOutput{Volumes: []*ec2.Volume{{VolumeId: aws.String(id)}}})
		_ = m.Respond(out)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	out, err := DescribeVolumeAttribute(t.Context(), &ec2.DescribeVolumeAttributeInput{
		VolumeId: aws.String(id), Attribute: aws.String(ec2.VolumeAttributeNameAutoEnableIo),
	}, nc, "000000000042")
	require.NoError(t, err)
	assert.Equal(t, id, aws.StringValue(out.VolumeId))
	require.NotNil(t, out.AutoEnableIO)
	assert.False(t, aws.BoolValue(out.AutoEnableIO.Value))
	assert.Nil(t, out.ProductCodes)

	_, err = DescribeVolumeAttribute(t.Context(), &ec2.DescribeVolumeAttributeInput{
		VolumeId: aws.String("vol-0123456789abcdef1"), Attribute: aws.String(ec2.VolumeAttributeNameAutoEnableIo),
	}, nc, "000000000042")
	assert.EqualError(t, err, awserrors.ErrorInvalidVolumeNotFound)
}
