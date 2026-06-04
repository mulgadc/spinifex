package gateway_elbv2

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

// DescribeListenerCertificates handles the ELBv2 DescribeListenerCertificates
// API call. It returns the certificates attached to a listener.
func DescribeListenerCertificates(input *elbv2.DescribeListenerCertificatesInput, natsConn *nats.Conn, accountID string) (elbv2.DescribeListenerCertificatesOutput, error) {
	var output elbv2.DescribeListenerCertificatesOutput

	if input == nil || input.ListenerArn == nil || *input.ListenerArn == "" {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.DescribeListenerCertificates(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
