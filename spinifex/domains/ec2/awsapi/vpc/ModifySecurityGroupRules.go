package vpc

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ModifySecurityGroupRules implements the EC2 ModifySecurityGroupRules action. It requires
// GroupId and at least one rule, and rejects malformed rule IDs before calling NATS.
func ModifySecurityGroupRules(ctx context.Context, input *ec2.ModifySecurityGroupRulesInput, natsConn *nats.Conn, accountID string) (ec2.ModifySecurityGroupRulesOutput, error) {
	var output ec2.ModifySecurityGroupRulesOutput
	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if aws.StringValue(input.GroupId) == "" || len(input.SecurityGroupRules) == 0 {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}
	// Only the malformed half is decidable here; whether a well-formed ID
	// exists is the handler's to answer.
	for _, u := range input.SecurityGroupRules {
		if u != nil && u.SecurityGroupRuleId != nil && ec2vpc.SGRuleIDIsMalformed(*u.SecurityGroupRuleId) {
			return output, errors.New(awserrors.ErrorInvalidSecurityGroupRuleIdMalformed)
		}
	}
	svc := ec2vpc.NewNATSVPCService(natsConn)
	result, err := svc.ModifySecurityGroupRules(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
