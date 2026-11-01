package awsgw

import (
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

func composeWithSQS(t *testing.T, host string) map[string]dispatch.Inventory {
	t.Helper()
	regs := append(sqsRegistrations(&sqsrelay.Relay{}, host, "9999"), sqsworkerapi.NewRegistration(sqsworkerapi.Deps{}))
	gw, err := composeGateway(gatewayInputs{
		Credentials:  credverify.New(nil, nil),
		Admission:    accountadmission.New(nil),
		PublicRoutes: PublicRoutes(nil, nil),
	}, regs...)
	require.NoError(t, err)
	return gw.Services.Inventory()
}

// The public service composes beside the worker's internal one and serves
// exactly the actions its capability profile classifies as implemented.
func TestSQSRegistration_ServesTheProfiledActionsBesideTheWorker(t *testing.T) {
	inv := composeWithSQS(t, "10.0.0.1")
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
	assert.NotContains(t, composeWithSQS(t, ""), sqsawsapi.ServiceName)
}
