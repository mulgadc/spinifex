package awsgw

import (
	"log/slog"
	"net"
	"net/url"
	"strings"

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
		Endpoint: sqsEndpoint(host, port),
	})}
}

// sqsEndpoint is the HTTPS base URL for host and port, bracketing an IPv6
// literal and omitting the standard port 443.
func sqsEndpoint(host, port string) string {
	u := url.URL{Scheme: "https", Host: net.JoinHostPort(host, port)}
	if port == "" || port == "443" {
		u.Host = host
		if strings.Contains(host, ":") {
			u.Host = "[" + host + "]"
		}
	}
	return u.String()
}

// sqsServed reports whether the composed registry serves the public SQS API.
func sqsServed(services *dispatch.Registry) bool {
	if services == nil {
		return false
	}
	_, ok := services.Lookup(sqsawsapi.ServiceName)
	return ok
}
