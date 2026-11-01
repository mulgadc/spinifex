package pipeline

import (
	"context"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	rdsinstance "github.com/mulgadc/spinifex/spinifex/domains/rds/instance"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRDSRequest_CreateDBInstance_EmptyMasterUserPasswordReturns400 runs the real public path:
// a CreateDBInstance query request through RDS_Request, over NATS, into the production
// rdsinstance.Service, and the resulting error through ErrorHandler's HTTP/XML rendering. AWS
// returns 400 InvalidParameterValue for a create with no MasterUserPassword.
func TestRDSRequest_CreateDBInstance_EmptyMasterUserPasswordReturns400(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	svc := rdsinstance.NewService(nc, "ap-southeast-2")
	sub, err := nc.Subscribe(rdsinstance.SubjectCreateDBInstance, func(msg *nats.Msg) {
		natsmsg.ServeNATSRequestCtx(msg, func(ctx context.Context, in *rds.CreateDBInstanceInput) (*rds.CreateDBInstanceOutput, error) {
			return svc.CreateDBInstance(ctx, in, natsmsg.AccountIDFromMsg(msg))
		})
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	gw := withVerifier(gwfix.WithDeps(&Config{DisableLogging: true}, gwfix.Deps{NATS: nc, IAM: allowAllIAMService()}))
	req := setupRDSRequest("Action=CreateDBInstance&DBInstanceIdentifier=orders-db&Engine=postgres&" +
		"DBInstanceClass=db.t3.medium&AllocatedStorage=20&MasterUsername=appuser&DBName=orders")

	reqErr := gw.serveRDS(httptest.NewRecorder(), req)
	require.Error(t, reqErr)

	w := httptest.NewRecorder()
	gw.ErrorHandler(w, req, reqErr)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "<Code>"+awserrors.ErrorInvalidParameterValue+"</Code>")
}

func TestRDSRequest_MalformedScalarReturnsHTTP400(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	gw := withVerifier(gwfix.WithDeps(&Config{DisableLogging: true}, gwfix.Deps{NATS: nc, IAM: allowAllIAMService()}))
	for _, body := range []string{
		"Action=CreateDBInstance&AllocatedStorage=abc",
		"Action=CreateDBInstance&MultiAZ=yes",
	} {
		t.Run(body, func(t *testing.T) {
			req := setupRDSRequest(body)
			err := gw.serveRDS(httptest.NewRecorder(), req)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())

			w := httptest.NewRecorder()
			gw.ErrorHandler(w, req, err)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), "<Code>"+awserrors.ErrorInvalidParameterValue+"</Code>")
		})
	}
}
