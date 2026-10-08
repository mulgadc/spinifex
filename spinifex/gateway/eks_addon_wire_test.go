package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/internal/testkit"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	gateway_eks "github.com/mulgadc/spinifex/spinifex/gateway/eks"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The body the CP agent receives is built by the SDK REST-JSON marshaller,
// which ignores the json tags: field names are Go names and empty strings are
// sent. The agent decodes it only because encoding/json matches case-insensitively.
func TestEKSRequest_InternalAddonsWireBody(t *testing.T) {
	const cpInstanceID = "i-cp0000000000001"
	_, nc, js := testutil.StartTestJetStream(t)
	kv, err := handlers_eks.GetOrCreateAccountBucket(t.Context(), js, authzAccountID)
	require.NoError(t, err)
	require.NoError(t, handlers_eks.PutClusterMeta(t.Context(), kv, &handlers_eks.ClusterMeta{
		Name:              "prod",
		Status:            handlers_eks.ClusterStatusActive,
		ControlPlaneNodes: []handlers_eks.ControlPlaneNode{{InstanceID: cpInstanceID}},
	}))

	svc, err := handlers_eks.NewEKSServiceImpl(handlers_eks.EKSServiceDeps{NATSConn: nc, Region: authzRegion})
	require.NoError(t, err)
	sub, err := nc.Subscribe("eks.ListStagedAddonManifests", func(m *nats.Msg) {
		acct := natsmsg.AccountIDFromMsg(m)
		natsmsg.ServeNATSRequestCtx(m, func(ctx context.Context, in *handlers_eks.ListStagedAddonManifestsInput) (*handlers_eks.ListStagedAddonManifestsOutput, error) {
			return svc.ListStagedAddonManifests(ctx, in, acct)
		})
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	for _, in := range []*eks.CreateAddonInput{
		{AddonName: aws.String("spinifex-noop")},
		{
			AddonName:             aws.String("aws-load-balancer-controller"),
			ServiceAccountRoleArn: aws.String("arn:aws:iam::123456789012:role/alb"),
			ConfigurationValues:   aws.String(`{"replicaCount":2}`),
		},
	} {
		in.ClusterName = aws.String("prod")
		_, err := svc.CreateAddon(t.Context(), in, authzAccountID)
		require.NoError(t, err)
	}

	gw := scopedPolicyGateway(statement("Allow", "eks:*", "*"))
	gw.NATSConn = nc
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/clusters/prod/internal-addons/"+authzAccountID, nil)
	ctx := context.WithValue(req.Context(), ctxService, "eks")
	ctx = context.WithValue(ctx, ctxAccountID, awsidentifiers.GlobalAccountID)
	ctx = context.WithValue(ctx, ctxIdentity, cpInstanceID)
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeAssumedRole)
	ctx = context.WithValue(ctx, ctxUnderlyingRoleARN,
		"arn:aws:iam::"+awsidentifiers.GlobalAccountID+":role/"+handlers_eks.CPInstanceRoleName)
	ctx = context.WithValue(ctx, ctxAssumedRoleARN,
		"arn:aws:sts::"+awsidentifiers.GlobalAccountID+":assumed-role/"+handlers_eks.CPInstanceRoleName+"/"+cpInstanceID)
	require.NoError(t, gw.EKS_Request(rec, req.WithContext(ctx)))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, `{"Addons":[`+
		`{"AddonName":"aws-load-balancer-controller","AddonVersion":"2.11.0",`+
		`"ServiceAccountRoleArn":"arn:aws:iam::123456789012:role/alb","ConfigurationValues":"{\"replicaCount\":2}"},`+
		`{"AddonName":"spinifex-noop","AddonVersion":"0.1.0","ServiceAccountRoleArn":"","ConfigurationValues":""}]}`,
		rec.Body.String())
}

// Only CreateCluster and CreateNodegroup hand a role to iam:PassRole; the add-on
// service-account role is stored and rendered without that check.
func TestPassedRoleARNs_AddonServiceAccountRoleIsNotChecked(t *testing.T) {
	body := []byte(`{"addonName":"aws-load-balancer-controller","serviceAccountRoleArn":"arn:aws:iam::123456789012:role/alb"}`)
	assert.Empty(t, gateway_eks.PassedRoleARNs("CreateAddon", body))
	assert.Empty(t, gateway_eks.PassedRoleARNs("UpdateAddon", body))
	assert.Equal(t, []string{"arn:aws:iam::123456789012:role/node"},
		gateway_eks.PassedRoleARNs("CreateNodegroup", []byte(`{"nodeRole":"arn:aws:iam::123456789012:role/node"}`)))
}
