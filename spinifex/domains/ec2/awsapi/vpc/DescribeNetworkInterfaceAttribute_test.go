package vpc

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescribeNetworkInterfaceAttribute_Attachment(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	const id = "eni-0123456789abcdef0"
	attachment := &ec2.NetworkInterfaceAttachment{
		AttachTime:          aws.Time(time.Date(2026, 11, 11, 0, 29, 23, 0, time.UTC)),
		AttachmentId:        aws.String("eni-attach-0123456789abcdef0"),
		DeleteOnTermination: aws.Bool(false),
		DeviceIndex:         aws.Int64(1),
		NetworkCardIndex:    aws.Int64(0),
		InstanceId:          aws.String("i-0123456789abcdef0"),
		InstanceOwnerId:     aws.String("000000000042"),
		Status:              aws.String(ec2.AttachmentStatusAttached),
	}
	sub, err := nc.Subscribe("ec2.DescribeNetworkInterfaces", func(m *nats.Msg) {
		var in ec2.DescribeNetworkInterfacesInput
		if json.Unmarshal(m.Data, &in) != nil || len(in.NetworkInterfaceIds) != 1 || *in.NetworkInterfaceIds[0] != id {
			_ = m.Respond(awserrors.GenerateErrorPayload(awserrors.ErrorInvalidNetworkInterfaceIDNotFound))
			return
		}
		out, _ := json.Marshal(ec2.DescribeNetworkInterfacesOutput{NetworkInterfaces: []*ec2.NetworkInterface{{
			NetworkInterfaceId: aws.String(id),
			Attachment:         attachment,
			Groups:             []*ec2.GroupIdentifier{{GroupId: aws.String("sg-0123456789abcdef0")}},
		}}})
		_ = m.Respond(out)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	out, err := DescribeNetworkInterfaceAttribute(t.Context(), &ec2.DescribeNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String(id), Attribute: aws.String(ec2.NetworkInterfaceAttributeAttachment),
	}, nc, "000000000042")
	require.NoError(t, err)
	assert.Equal(t, id, aws.StringValue(out.NetworkInterfaceId))
	assert.Equal(t, attachment, out.Attachment)
	assert.Nil(t, out.Groups, "only the requested attribute is answered")

	_, err = DescribeNetworkInterfaceAttribute(t.Context(), &ec2.DescribeNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String("eni-0123456789abcdef1"), Attribute: aws.String(ec2.NetworkInterfaceAttributeAttachment),
	}, nc, "000000000042")
	assert.EqualError(t, err, awserrors.ErrorInvalidNetworkInterfaceIDNotFound)
}
