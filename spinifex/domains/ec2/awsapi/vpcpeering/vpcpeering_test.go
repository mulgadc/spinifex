package vpcpeering

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "123456789012"

func TestNilInputRefused(t *testing.T) {
	ctx := t.Context()
	_, err := CreateVpcPeeringConnection(ctx, nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
	_, err = AcceptVpcPeeringConnection(ctx, nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
	_, err = RejectVpcPeeringConnection(ctx, nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
	_, err = DeleteVpcPeeringConnection(ctx, nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
	_, err = ModifyVpcPeeringConnectionOptions(ctx, nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
	_, err = DescribeVpcPeeringConnections(ctx, nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestMissingConnectionID(t *testing.T) {
	ctx := t.Context()
	for name, call := range map[string]func(id *string) error{
		"Accept": func(id *string) error {
			_, err := AcceptVpcPeeringConnection(ctx, &ec2.AcceptVpcPeeringConnectionInput{VpcPeeringConnectionId: id}, nil, testAccountID)
			return err
		},
		"Reject": func(id *string) error {
			_, err := RejectVpcPeeringConnection(ctx, &ec2.RejectVpcPeeringConnectionInput{VpcPeeringConnectionId: id}, nil, testAccountID)
			return err
		},
		"Delete": func(id *string) error {
			_, err := DeleteVpcPeeringConnection(ctx, &ec2.DeleteVpcPeeringConnectionInput{VpcPeeringConnectionId: id}, nil, testAccountID)
			return err
		},
		"Modify": func(id *string) error {
			_, err := ModifyVpcPeeringConnectionOptions(ctx, &ec2.ModifyVpcPeeringConnectionOptionsInput{VpcPeeringConnectionId: id}, nil, testAccountID)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, id := range []*string{nil, aws.String("")} {
				code, msg, ok := awserrors.ResolveErrorDetail(call(id))
				require.True(t, ok)
				assert.Equal(t, awserrors.ErrorMissingParameter, code)
				assert.Equal(t, "The request must contain the parameter vpcPeeringConnectionId", msg)
			}
		})
	}
}
