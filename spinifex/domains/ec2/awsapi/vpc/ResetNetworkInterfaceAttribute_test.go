package vpc

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveENIDescribes answers ec2.DescribeNetworkInterfaces from a real VPC
// service, scoped by the caller's account header as the daemon scopes it.
func serveENIDescribes(t *testing.T) (*ec2vpc.VPCServiceImpl, *nats.Conn) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	testutil.StubVpcdSGResponder(t, nc)
	svc, err := ec2vpc.NewVPCServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	sub, err := nc.Subscribe("ec2.DescribeNetworkInterfaces", func(m *nats.Msg) {
		var in ec2.DescribeNetworkInterfacesInput
		_ = json.Unmarshal(m.Data, &in)
		out, err := svc.DescribeNetworkInterfaces(t.Context(), &in, natsmsg.AccountIDFromMsg(m))
		if err != nil {
			_, message, _ := awserrors.ResolveErrorDetail(err)
			_ = m.Respond(awserrors.GenerateErrorPayloadWithMessage(awserrors.ValidErrorCodeFromError(err), message))
			return
		}
		data, _ := json.Marshal(out)
		_ = m.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return svc, nc
}

func TestResetNetworkInterfaceAttribute(t *testing.T) {
	svc, nc := serveENIDescribes(t)
	ctx := t.Context()
	vpc, err := svc.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")}, testAccountID)
	require.NoError(t, err)
	subnet, err := svc.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: vpc.Vpc.VpcId, CidrBlock: aws.String("10.0.1.0/24")}, testAccountID)
	require.NoError(t, err)
	eni, err := svc.CreateNetworkInterface(ctx, &ec2.CreateNetworkInterfaceInput{SubnetId: subnet.Subnet.SubnetId}, testAccountID)
	require.NoError(t, err)
	eniID := aws.StringValue(eni.NetworkInterface.NetworkInterfaceId)

	reset := func(id string, sourceDestCheck *string, accountID string) error {
		_, err := ResetNetworkInterfaceAttribute(ctx, &ec2.ResetNetworkInterfaceAttributeInput{
			NetworkInterfaceId: aws.String(id), SourceDestCheck: sourceDestCheck,
		}, nc, accountID)
		return err
	}

	cases := []struct {
		name            string
		id              string
		sourceDestCheck *string
		accountID       string
		code, message   string
	}{
		{"absent attribute, checked before the id", "eni-zzz", nil, testAccountID,
			awserrors.ErrorInvalidParameterCombination, "No attributes specified."},
		{"non-empty attribute, checked before the id", "eni-zzz", aws.String("true"), testAccountID,
			awserrors.ErrorInvalidRequest, "The request received was invalid."},
		{"malformed id", "eni-zzz", aws.String(""), testAccountID,
			awserrors.ErrorInvalidNetworkInterfaceIdMalformed, `Invalid id: "eni-zzz"`},
		{"missing id", "eni-0f3a9c2b7d1e4a5b6", aws.String(""), testAccountID,
			awserrors.ErrorInvalidNetworkInterfaceIDNotFound, "The networkInterface ID 'eni-0f3a9c2b7d1e4a5b6' does not exist"},
		{"another account's interface", eniID, aws.String(""), "210987654321",
			awserrors.ErrorInvalidNetworkInterfaceIDNotFound, "The networkInterface ID '" + eniID + "' does not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := reset(tc.id, tc.sourceDestCheck, tc.accountID)
			require.Error(t, err)
			code, message, _ := awserrors.ResolveErrorDetail(err)
			assert.Equal(t, tc.code, code)
			assert.Equal(t, tc.message, message)
		})
	}

	require.NoError(t, reset(eniID, aws.String(""), testAccountID))
	attr, err := DescribeNetworkInterfaceAttribute(ctx, &ec2.DescribeNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String(eniID), Attribute: aws.String(ec2.NetworkInterfaceAttributeSourceDestCheck),
	}, nc, testAccountID)
	require.NoError(t, err)
	assert.True(t, aws.BoolValue(attr.SourceDestCheck.Value))
}
