package gateway

import (
	ec2awsapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"net/http"
	"testing"
)

// The gateway has no NATS connection, so any request that reached its handler
// would fail ServerInternal; DryRunOperation proves nothing was dispatched.
func TestEC2Request_DryRunAnswersEveryAction(t *testing.T) {
	gw := withVerifier(&GatewayConfig{DisableLogging: true, Region: authzRegion, IAMService: allowAllIAMService()})

	actions := ec2awsapi.ActionNames()

	for _, action := range actions {
		t.Run(action, func(t *testing.T) {
			w := serveEC2(gw, "Action="+action+"&DryRun=true")
			if !ec2awsapi.InputHasField(action, "DryRun") {
				assert.NotContains(t, w.Body.String(), awserrors.ErrorDryRunOperation)
				return
			}
			assert.Equal(t, http.StatusPreconditionFailed, w.Code)
			assert.Contains(t, w.Body.String(), "<Code>DryRunOperation</Code>")
			assert.Contains(t, w.Body.String(), "<Message>"+dryRunMessage+"</Message>")
		})
	}
}
