package snapshot

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

func TestDescribeSnapshotAttribute_CreateVolumePermission(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	const id = "snap-0123456789abcdef0"
	sub, err := nc.Subscribe("ec2.DescribeSnapshots", func(m *nats.Msg) {
		var in ec2.DescribeSnapshotsInput
		if json.Unmarshal(m.Data, &in) != nil || len(in.SnapshotIds) != 1 || *in.SnapshotIds[0] != id {
			_ = m.Respond(awserrors.GenerateErrorPayload(awserrors.ErrorInvalidSnapshotNotFound))
			return
		}
		out, _ := json.Marshal(ec2.DescribeSnapshotsOutput{Snapshots: []*ec2.Snapshot{{SnapshotId: aws.String(id)}}})
		_ = m.Respond(out)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	out, err := DescribeSnapshotAttribute(t.Context(), &ec2.DescribeSnapshotAttributeInput{
		SnapshotId: aws.String(id), Attribute: aws.String(ec2.SnapshotAttributeNameCreateVolumePermission),
	}, nc, "000000000042")
	require.NoError(t, err)
	assert.Equal(t, id, aws.StringValue(out.SnapshotId))
	assert.NotNil(t, out.CreateVolumePermissions, "AWS renders an empty list for a private snapshot")
	assert.Empty(t, out.CreateVolumePermissions)
	assert.Nil(t, out.ProductCodes)

	_, err = DescribeSnapshotAttribute(t.Context(), &ec2.DescribeSnapshotAttributeInput{
		SnapshotId: aws.String("snap-0123456789abcdef1"), Attribute: aws.String(ec2.SnapshotAttributeNameCreateVolumePermission),
	}, nc, "000000000042")
	assert.EqualError(t, err, awserrors.ErrorInvalidSnapshotNotFound)
}
