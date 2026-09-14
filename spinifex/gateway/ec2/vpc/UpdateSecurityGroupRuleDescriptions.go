package gateway_ec2_vpc

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/nats-io/nats.go"
)

// validateSGRuleDescriptions checks the sgr-<17 hex> shape before the request
// crosses NATS so a malformed ID never reaches the handler.
func validateSGRuleDescriptions(descriptions []*ec2.SecurityGroupRuleDescription) error {
	for _, d := range descriptions {
		if d == nil || d.SecurityGroupRuleId == nil || !handlers_ec2_vpc.SGRuleIDRegex.MatchString(*d.SecurityGroupRuleId) {
			return errors.New(awserrors.ErrorInvalidSecurityGroupRuleIdMalformed)
		}
	}
	return nil
}

func UpdateSecurityGroupRuleDescriptionsIngress(ctx context.Context, input *ec2.UpdateSecurityGroupRuleDescriptionsIngressInput, natsConn *nats.Conn, accountID string) (ec2.UpdateSecurityGroupRuleDescriptionsIngressOutput, error) {
	var output ec2.UpdateSecurityGroupRuleDescriptionsIngressOutput
	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.GroupId == nil || *input.GroupId == "" {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}
	if err := validateSGRuleDescriptions(input.SecurityGroupRuleDescriptions); err != nil {
		return output, err
	}
	svc := handlers_ec2_vpc.NewNATSVPCService(natsConn)
	result, err := svc.UpdateSecurityGroupRuleDescriptionsIngress(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}

func UpdateSecurityGroupRuleDescriptionsEgress(ctx context.Context, input *ec2.UpdateSecurityGroupRuleDescriptionsEgressInput, natsConn *nats.Conn, accountID string) (ec2.UpdateSecurityGroupRuleDescriptionsEgressOutput, error) {
	var output ec2.UpdateSecurityGroupRuleDescriptionsEgressOutput
	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.GroupId == nil || *input.GroupId == "" {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}
	if err := validateSGRuleDescriptions(input.SecurityGroupRuleDescriptions); err != nil {
		return output, err
	}
	svc := handlers_ec2_vpc.NewNATSVPCService(natsConn)
	result, err := svc.UpdateSecurityGroupRuleDescriptionsEgress(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
