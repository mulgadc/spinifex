package awsgw

import (
	"net/url"
	"testing"

	sqsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi"
	sqsworkerapi "github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi/worker"
	sqsrelay "github.com/mulgadc/spinifex/spinifex/domains/sqs/relay"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/accountadmission"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func composeWithSQS(t *testing.T, host string) *dispatch.Registry {
	t.Helper()
	regs := append(sqsRegistrations(&sqsrelay.Relay{}, host, "9999"), sqsworkerapi.NewRegistration(sqsworkerapi.Deps{}))
	gw, err := composeGateway(gatewayInputs{
		Credentials:  credverify.New(nil, nil),
		Admission:    accountadmission.New(nil),
		PublicRoutes: PublicRoutes(nil, nil),
	}, regs...)
	require.NoError(t, err)
	return gw.Services
}

// The public service composes beside the worker's internal one and serves
// exactly the actions its capability profile classifies as implemented.
func TestSQSRegistration_ServesTheProfiledActionsBesideTheWorker(t *testing.T) {
	services := composeWithSQS(t, "10.0.0.1")
	assert.True(t, sqsServed(services), "status must report the composed SQS API")
	inv := services.Inventory()
	require.Contains(t, inv, sqsawsapi.ServiceName)
	assert.ElementsMatch(t, []string{
		"ChangeMessageVisibility", "ChangeMessageVisibilityBatch", "CreateQueue", "DeleteMessage",
		"DeleteMessageBatch", "GetQueueAttributes", "GetQueueUrl", "ListQueueTags", "ListQueues",
		"ReceiveMessage", "SendMessage", "SendMessageBatch", "SetQueueAttributes", "TagQueue", "UntagQueue",
	}, inv[sqsawsapi.ServiceName].Registered)
}

// Without a concrete host every queue URL would be undialable, so the public
// service is left out rather than served with broken URLs.
func TestSQSRegistration_NoConcreteHostRegistersNothing(t *testing.T) {
	services := composeWithSQS(t, "")
	assert.NotContains(t, services.Inventory(), sqsawsapi.ServiceName)
	assert.False(t, sqsServed(services), "status must not report an API the gateway does not serve")
	assert.False(t, sqsServed(nil), "before composition nothing is served")
}

// Queue URLs must parse back to the gateway's host and port, including an
// IPv6 listen address, which needs brackets.
func TestSQSEndpoint_IsADialableURL(t *testing.T) {
	for _, tc := range []struct{ host, port, want string }{
		{"10.0.0.1", "9999", "https://10.0.0.1:9999"},
		{"fd00::1", "9999", "https://[fd00::1]:9999"},
		{"fd00::1", "443", "https://[fd00::1]"},
		{"gw.example", "443", "https://gw.example"},
	} {
		got := sqsEndpoint(tc.host, tc.port)
		assert.Equal(t, tc.want, got)
		u, err := url.Parse(got + "/000000000000/q")
		require.NoError(t, err, got)
		assert.Equal(t, tc.host, u.Hostname(), got)
	}
}
