package gateway_ec2_vpc

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/nats-io/nats.go"
)

// Nil input is valid and treated as "all rules in the account"; supplied
// SecurityGroupRuleIds are validated against the sgr-<17 hex> shape before the
// request crosses NATS so a malformed ID never hits the handler.
func DescribeSecurityGroupRules(input *ec2.DescribeSecurityGroupRulesInput, natsConn *nats.Conn, accountID string) (ec2.DescribeSecurityGroupRulesOutput, error) {
	var output ec2.DescribeSecurityGroupRulesOutput
	if input != nil {
		for _, id := range input.SecurityGroupRuleIds {
			if id == nil || !handlers_ec2_vpc.SGRuleIDRegex.MatchString(*id) {
				return output, errors.New(awserrors.ErrorInvalidSecurityGroupRuleIdMalformed)
			}
		}
	}
	svc := handlers_ec2_vpc.NewNATSVPCService(natsConn)
	result, err := svc.DescribeSecurityGroupRules(input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
