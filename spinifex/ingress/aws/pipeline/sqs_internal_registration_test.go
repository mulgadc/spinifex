package pipeline

import (
	"context"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"net/http"
	"net/http/httptest"
	"testing"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	rdsawsapi "github.com/mulgadc/spinifex/spinifex/domains/rds/awsapi"
	sqsworkerapi "github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi/worker"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/firststore"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storelaunch"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sqsWorkerInstance = "i-0abc12345def67890"

type sqsWorkerIdentities struct{ asked int }

func (s *sqsWorkerIdentities) Authorize(_ context.Context, instanceID string) (workerident.Identity, error) {
	s.asked++
	return workerident.Identity{Region: "ap-southeast-2", InstanceID: instanceID, Generation: 1, ClaimID: "claim-0001"}, nil
}

type sqsWorkerFirstStore struct{}

func (sqsWorkerFirstStore) Claim(context.Context, string, firststore.Identity) (firststore.Decision, error) {
	return firststore.Decision{CreateKey: true}, nil
}

func (sqsWorkerFirstStore) RecordPublicKey(context.Context, string, firststore.Identity, string) error {
	return nil
}

type sqsWorkerStores struct{}

func (sqsWorkerStores) Current(context.Context, string) (storelaunch.Record, error) {
	return storelaunch.Record{}, jetstream.ErrKeyNotFound
}

// withSQSInternal registers the internal SQS surface beside RDS, as awsgw
// composes them, so routing is selected by signing service.
func withSQSInternal(gw *Config, deps sqsworkerapi.Deps) *Config {
	b := dispatch.NewBuilder()
	for _, reg := range []dispatch.Registration{rdsawsapi.NewRegistration(rdsawsapi.Deps{}), sqsworkerapi.NewRegistration(deps)} {
		if err := b.Register(reg); err != nil {
			panic(err)
		}
	}
	gw.Services = b.Build()
	if err := gw.ValidateServices(); err != nil {
		panic(err)
	}
	return gw
}

func sqsWorkerRequest(service, body, session string) *http.Request {
	req := setupRDSRequest(body)
	roleARN := "arn:aws:iam::" + awsidentifiers.GlobalAccountID + ":role/" + sqsworkerapi.WorkerRoleName
	ctx := reqctx.WithService(req.Context(), service)
	ctx = withPrincipalField(ctx, fieldAccountID, awsidentifiers.GlobalAccountID)
	ctx = withPrincipalField(ctx, fieldIdentity, session)
	ctx = withPrincipalField(ctx, fieldPrincipalType, gwfix.PrincipalTypeAssumedRole)
	ctx = withPrincipalField(ctx, fieldUnderlyingRoleARN, roleARN)
	ctx = withPrincipalField(ctx, fieldAssumedRoleARN,
		"arn:aws:sts::"+awsidentifiers.GlobalAccountID+":assumed-role/"+sqsworkerapi.WorkerRoleName+"/"+session)
	return req.WithContext(ctx)
}

func serveSigned(gw *Config, w http.ResponseWriter, r *http.Request) error {
	entry, ok := gw.registered(requestService(r))
	if !ok {
		panic("no registration serves " + requestService(r))
	}
	return gw.dispatchRegistered(entry, w, r)
}

func newSQSInternalGateway(policies []iamdomain.PolicyDocument) (*Config, *rdsPolicyProbe, *sqsWorkerIdentities) {
	gw, probe := newRDSPolicyGateway(policies)
	ids := &sqsWorkerIdentities{}
	return withSQSInternal(gw, sqsworkerapi.Deps{Identities: ids, FirstStore: sqsWorkerFirstStore{}, Stores: sqsWorkerStores{}}), probe, ids
}

// The worker's session, signed for spinifex-sqs, reaches the internal surface
// and is evaluated against its role policy under the spinifex-sqs prefix.
func TestSQSInternal_RoutedBySigningServiceAndRolePolicy(t *testing.T) {
	gw, probe, ids := newSQSInternalGateway(gwfix.AllowPolicy("spinifex-sqs:ClaimFirstStore"))
	rec := httptest.NewRecorder()
	require.NoError(t, serveSigned(gw, rec, sqsWorkerRequest(sqsworkerapi.ServiceName, "Action=ClaimFirstStore", sqsWorkerInstance)))
	assert.Equal(t, 1, probe.consulted)
	assert.Equal(t, 1, ids.asked)
	assert.Contains(t, rec.Body.String(), "<CreateKey>true</CreateKey>")
}

// The role policy is real: a worker session whose policy grants another
// service's action is denied before any store is consulted.
func TestSQSInternal_PolicyForAnotherServiceDenies(t *testing.T) {
	gw, probe, ids := newSQSInternalGateway(gwfix.AllowPolicy("sqs:ClaimFirstStore"))
	err := serveSigned(gw, httptest.NewRecorder(), sqsWorkerRequest(sqsworkerapi.ServiceName, "Action=ClaimFirstStore", sqsWorkerInstance))
	assertDenied(t, err)
	assert.Equal(t, 1, probe.consulted)
	assert.Zero(t, ids.asked)
}

// A customer administrator holding every action is refused by the class gate.
func TestSQSInternal_CustomerAdminDenied(t *testing.T) {
	gw, probe, ids := newSQSInternalGateway(gwfix.AllowPolicy("*"))
	req := setupRDSUserRequest("Action=ClaimFirstStore", rdsTestAccountID)
	req = req.WithContext(reqctx.WithService(req.Context(), sqsworkerapi.ServiceName))
	assertDenied(t, serveSigned(gw, httptest.NewRecorder(), req))
	assert.Zero(t, probe.consulted)
	assert.Zero(t, ids.asked)
}

// Public SQS is untouched: nothing serves the sqs signing service, and the
// internal actions do not answer under RDS's name either.
func TestSQSInternal_PublicSQSUnchanged(t *testing.T) {
	gw, _, ids := newSQSInternalGateway(gwfix.AllowPolicy("*"))
	_, ok := gw.registered("sqs")
	assert.False(t, ok, "the sqs signing service must stay unserved")
	inv := gw.Services.Inventory()
	assert.NotContains(t, inv, "sqs")
	assert.ElementsMatch(t, sqsworkerapi.ActionNames(), inv[sqsworkerapi.ServiceName].Registered)

	err := serveSigned(gw, httptest.NewRecorder(), sqsWorkerRequest(rdsawsapi.ServiceName, "Action=ClaimFirstStore", sqsWorkerInstance))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
	assert.Zero(t, ids.asked)
}
