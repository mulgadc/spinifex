package gateway_elbv2

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// RemoveListenerCertificates handles the ELBv2 RemoveListenerCertificates API
// call. It detaches the named certificates from a listener.
func RemoveListenerCertificates(ctx context.Context, input *elbv2.RemoveListenerCertificatesInput, natsConn *nats.Conn, accountID string) (elbv2.RemoveListenerCertificatesOutput, error) {
	var output elbv2.RemoveListenerCertificatesOutput

	if input == nil || input.ListenerArn == nil || *input.ListenerArn == "" || len(input.Certificates) == 0 {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}

	svc := elbv2domain.NewNATSELBv2Service(natsConn)
	result, err := svc.RemoveListenerCertificates(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
