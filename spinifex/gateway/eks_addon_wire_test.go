package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/internal/testkit"
	eksawsapi "github.com/mulgadc/spinifex/spinifex/domains/eks/awsapi"
	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
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
	kv, err := ekscluster.GetOrCreateAccountBucket(t.Context(), js, authzAccountID)
	require.NoError(t, err)
	require.NoError(t, ekscluster.PutClusterMeta(t.Context(), kv, &ekscluster.ClusterMeta{
		Name:              "prod",
		Status:            ekscluster.ClusterStatusActive,
		ControlPlaneNodes: []ekscluster.ControlPlaneNode{{InstanceID: cpInstanceID}},
	}))

	svc, err := ekscluster.NewEKSServiceImpl(ekscluster.EKSServiceDeps{NATSConn: nc, Region: authzRegion})
	require.NoError(t, err)
	sub, err := nc.Subscribe("eks.ListStagedAddonManifests", func(m *nats.Msg) {
		acct := natsmsg.AccountIDFromMsg(m)
		natsmsg.ServeNATSRequestCtx(m, func(ctx context.Context, in *ekscluster.ListStagedAddonManifestsInput) (*ekscluster.ListStagedAddonManifestsOutput, error) {
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
		"arn:aws:iam::"+awsidentifiers.GlobalAccountID+":role/"+ekscluster.CPInstanceRoleName)
	ctx = context.WithValue(ctx, ctxAssumedRoleARN,
		"arn:aws:sts::"+awsidentifiers.GlobalAccountID+":assumed-role/"+ekscluster.CPInstanceRoleName+"/"+cpInstanceID)
	require.NoError(t, gw.EKS_Request(rec, req.WithContext(ctx)))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, `{"Addons":[`+
		`{"AddonName":"aws-load-balancer-controller","AddonVersion":"2.11.0",`+
		`"ServiceAccountRoleArn":"arn:aws:iam::123456789012:role/alb","ConfigurationValues":"{\"replicaCount\":2}"},`+
		`{"AddonName":"spinifex-noop","AddonVersion":"0.1.0","ServiceAccountRoleArn":"","ConfigurationValues":""}]}`,
		rec.Body.String())
}

// CreateAddon and UpdateAddon hand their service-account role to iam:PassRole,
// same as CreateCluster's roleArn and CreateNodegroup's nodeRole; an absent
// role passes nothing.
func TestPassedRoleARNs_AddonServiceAccountRoleIsChecked(t *testing.T) {
	body := []byte(`{"addonName":"aws-load-balancer-controller","serviceAccountRoleArn":"arn:aws:iam::123456789012:role/alb"}`)
	assert.Equal(t, []string{"arn:aws:iam::123456789012:role/alb"}, eksawsapi.PassedRoleARNs("CreateAddon", body))
	assert.Equal(t, []string{"arn:aws:iam::123456789012:role/alb"}, eksawsapi.PassedRoleARNs("UpdateAddon", body))

	roleless := []byte(`{"addonName":"spinifex-noop"}`)
	assert.Empty(t, eksawsapi.PassedRoleARNs("CreateAddon", roleless))
	assert.Empty(t, eksawsapi.PassedRoleARNs("UpdateAddon", roleless))

	assert.Equal(t, []string{"arn:aws:iam::123456789012:role/node"},
		eksawsapi.PassedRoleARNs("CreateNodegroup", []byte(`{"nodeRole":"arn:aws:iam::123456789012:role/node"}`)))
}
