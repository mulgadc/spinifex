package gateway_ec2_vpc

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// Nil input is valid and treated as "all rules in the account"; supplied
// SecurityGroupRuleIds are validated against the sgr-<17 hex> shape before the
// request crosses NATS so a malformed ID never hits the handler.
func DescribeSecurityGroupRules(ctx context.Context, input *ec2.DescribeSecurityGroupRulesInput, natsConn *nats.Conn, accountID string) (ec2.DescribeSecurityGroupRulesOutput, error) {
	var output ec2.DescribeSecurityGroupRulesOutput
	if input != nil {
		for _, id := range input.SecurityGroupRuleIds {
			if id == nil || !ec2vpc.SGRuleIDRegex.MatchString(*id) {
				return output, errors.New(awserrors.ErrorInvalidSecurityGroupRuleIdMalformed)
			}
		}
	}
	svc := ec2vpc.NewNATSVPCService(natsConn)
	result, err := svc.DescribeSecurityGroupRules(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
