package awsgw

import (
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/domains/ecr"
	sqsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi"
	sqsrelay "github.com/mulgadc/spinifex/spinifex/domains/sqs/relay"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// sqsRegistrations serves the public SQS actions through the relay, with queue
// URLs on the gateway's own concrete host. Without one, SQS is not registered,
// because a queue URL a client cannot dial would break every later call.
func sqsRegistrations(r *sqsrelay.Relay, host, port string) []dispatch.Registration {
	if host == "" {
		slog.Warn("awsgw: SQS not registered: no concrete gateway host for queue URLs")
		return nil
	}
	return []dispatch.Registration{sqsawsapi.NewRegistration(sqsawsapi.Deps{
		Queues: r, Messages: r, Receives: r, Deletes: r, Visibility: r,
		Endpoint: "https://" + ecr.RegistryURIHost(host, port),
	})}
}
