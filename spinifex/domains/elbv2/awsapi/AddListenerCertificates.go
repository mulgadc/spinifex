package awsapi

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// AddListenerCertificates handles the ELBv2 AddListenerCertificates API call. It
// attaches additional SNI certificates to a secure listener.
func AddListenerCertificates(ctx context.Context, input *elbv2.AddListenerCertificatesInput, natsConn *nats.Conn, accountID string) (elbv2.AddListenerCertificatesOutput, error) {
	var output elbv2.AddListenerCertificatesOutput

	if input == nil || input.ListenerArn == nil || *input.ListenerArn == "" || len(input.Certificates) == 0 {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}

	svc := elbv2domain.NewNATSELBv2Service(natsConn)
	result, err := svc.AddListenerCertificates(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
