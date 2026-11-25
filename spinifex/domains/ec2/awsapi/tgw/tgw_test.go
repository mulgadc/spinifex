package tgw

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
)

const testAccountID = "123456789012"

func TestNilInputRefused(t *testing.T) {
	ctx := t.Context()
	_, err := CreateTransitGateway(ctx, nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
	_, err = DeleteTransitGateway(ctx, nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
	_, err = DescribeTransitGateways(ctx, nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
	_, err = CreateTransitGatewayVpcAttachment(ctx, nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
	_, err = DeleteTransitGatewayVpcAttachment(ctx, nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
	_, err = DescribeTransitGatewayVpcAttachments(ctx, nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}
