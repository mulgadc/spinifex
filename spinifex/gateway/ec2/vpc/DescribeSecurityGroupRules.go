package gateway_ec2_vpc

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/nats-io/nats.go"
)

// DescribeSecurityGroupRules treats a nil input as "all rules in the account". A supplied
// SecurityGroupRuleId AWS calls malformed is rejected before the request crosses NATS; whether a
// well-formed ID exists is the handler's to answer.
func DescribeSecurityGroupRules(ctx context.Context, input *ec2.DescribeSecurityGroupRulesInput, natsConn *nats.Conn, accountID string) (ec2.DescribeSecurityGroupRulesOutput, error) {
	var output ec2.DescribeSecurityGroupRulesOutput
	if input != nil {
		for _, id := range input.SecurityGroupRuleIds {
			if id == nil || handlers_ec2_vpc.SGRuleIDIsMalformed(*id) {
				return output, errors.New(awserrors.ErrorInvalidSecurityGroupRuleIdMalformed)
			}
		}
	}
	svc := handlers_ec2_vpc.NewNATSVPCService(natsConn)
	result, err := svc.DescribeSecurityGroupRules(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
