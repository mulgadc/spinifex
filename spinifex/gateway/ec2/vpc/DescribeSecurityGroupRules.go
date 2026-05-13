package gateway_ec2_vpc

import (
	"errors"
	"regexp"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/nats-io/nats.go"
)

// sgrIDRegex must stay in lockstep with utils.GenerateResourceID("sgr").
var sgrIDRegex = regexp.MustCompile(`^sgr-[0-9a-f]{17}$`)

// DescribeSecurityGroupRules handles the EC2 DescribeSecurityGroupRules API call.
// Nil input is valid and treated as "all rules in the account"; supplied
// SecurityGroupRuleIds are validated against the sgr-<17 hex> shape before the
// request crosses NATS so a malformed ID never hits the handler.
func DescribeSecurityGroupRules(input *ec2.DescribeSecurityGroupRulesInput, natsConn *nats.Conn, accountID string) (ec2.DescribeSecurityGroupRulesOutput, error) {
	var output ec2.DescribeSecurityGroupRulesOutput
	if input != nil {
		for _, id := range input.SecurityGroupRuleIds {
			if id == nil || !sgrIDRegex.MatchString(*id) {
				return output, errors.New(awserrors.ErrorInvalidParameterValue)
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
