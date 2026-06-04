package gateway_elbv2

import (
	"github.com/aws/aws-sdk-go/service/elbv2"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

// DescribeSSLPolicies handles the ELBv2 DescribeSSLPolicies API call. It serves
// the fixed catalog of supported security policies.
func DescribeSSLPolicies(input *elbv2.DescribeSSLPoliciesInput, natsConn *nats.Conn, accountID string) (elbv2.DescribeSSLPoliciesOutput, error) {
	var output elbv2.DescribeSSLPoliciesOutput

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.DescribeSSLPolicies(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
