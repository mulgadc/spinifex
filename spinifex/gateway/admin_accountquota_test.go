//test:in-package — drives the unexported admin handlers and the request identity they read from the context.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_quota "github.com/mulgadc/spinifex/spinifex/handlers/quota"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func quotaPtr(v int) *int { return &v }

// quotaIAMService resolves exactly one account, so a request naming any other
// exercises the "account must exist" gate.
type quotaIAMService struct {
	policyMockIAMService

	known string
}

func (m *quotaIAMService) GetAccount(accountID string) (*handlers_iam.Account, error) {
	if accountID != m.known {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &handlers_iam.Account{AccountID: accountID, Status: handlers_iam.AccountStatusActive}, nil
}

// quotaTestGateway wires a real quota Service over a real JetStream KV bucket,
// so the admin handlers are exercised against the store they use in production.
func quotaTestGateway(t *testing.T) (*GatewayConfig, string) {
	t.Helper()
	const account = "000000000042"

	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket:  handlers_quota.KVBucketAccountQuota,
		History: 1,
	})
	require.NoError(t, err)

	quota := handlers_quota.New(handlers_quota.Limits{
		Enabled: true, VCPUs: 16, VPCs: 4, Subnets: 16, EIPs: 4,
		Volumes: 16, VolumesGiB: 200, RDSInstances: 2, LoadBalancers: 2,
	}, nil)
	quota.SetOverrides(kv)

	return &GatewayConfig{
		DisableLogging: true,
		IAMService:     &quotaIAMService{known: account},
		Quota:          quota,
	}, account
}

func quotaRequestBody(t *testing.T, req AccountQuotaRequest) []byte {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	return body
}

// An account with no override must report the configured baseline, with every
// dimension attributed to the config.
func TestAdminGetAccountQuotaInherits(t *testing.T) {
	gw, account := quotaTestGateway(t)

	output, err := gw.adminGetAccountQuota(t.Context(), quotaRequestBody(t, AccountQuotaRequest{AccountID: account}))
	require.NoError(t, err)

	resp, ok := output.(*AccountQuotaResponse)
	require.True(t, ok)
	require.Equal(t, 16, resp.Limits["vcpus"])
	require.Equal(t, sourceConfig, resp.Source["vcpus"])
}

// A stored override must come back on the next read, with only the dimension it
// named changed.
func TestAdminPutThenGetAccountQuota(t *testing.T) {
	gw, account := quotaTestGateway(t)
	ctx := context.WithValue(t.Context(), ctxIdentity, "operator")

	output, err := gw.adminPutAccountQuota(ctx, quotaRequestBody(t, AccountQuotaRequest{
		AccountID: account,
		Overrides: handlers_quota.Overrides{VCPUs: quotaPtr(32)},
	}))
	require.NoError(t, err)

	put, ok := output.(*AccountQuotaResponse)
	require.True(t, ok)
	require.Equal(t, 32, put.Limits["vcpus"])
	require.Equal(t, sourceOverride, put.Source["vcpus"])
	require.Equal(t, "operator", put.Overrides.UpdatedBy)
	require.Equal(t, 4, put.Limits["vpcs"], "an unnamed dimension must keep inheriting")

	output, err = gw.adminGetAccountQuota(t.Context(), quotaRequestBody(t, AccountQuotaRequest{AccountID: account}))
	require.NoError(t, err)
	got, ok := output.(*AccountQuotaResponse)
	require.True(t, ok)
	require.Equal(t, 32, got.Limits["vcpus"])
	require.Equal(t, sourceOverride, got.Source["vcpus"])
}

// Clearing must return the account to the configured limits.
func TestAdminPutAccountQuotaClears(t *testing.T) {
	gw, account := quotaTestGateway(t)
	ctx := context.WithValue(t.Context(), ctxIdentity, "operator")

	_, err := gw.adminPutAccountQuota(ctx, quotaRequestBody(t, AccountQuotaRequest{
		AccountID: account,
		Overrides: handlers_quota.Overrides{VCPUs: quotaPtr(32)},
	}))
	require.NoError(t, err)

	output, err := gw.adminPutAccountQuota(ctx, quotaRequestBody(t, AccountQuotaRequest{AccountID: account}))
	require.NoError(t, err)

	resp, ok := output.(*AccountQuotaResponse)
	require.True(t, ok)
	require.Equal(t, 16, resp.Limits["vcpus"])
	require.Equal(t, sourceConfig, resp.Source["vcpus"])
}

// Storing an override against an account that does not exist would leave a
// record that never applies to anything, so a bad ID is refused.
func TestAdminAccountQuotaRejectsBadRequests(t *testing.T) {
	gw, _ := quotaTestGateway(t)

	tests := []struct {
		name string
		body []byte
	}{
		{"malformed json", []byte("{")},
		{"missing account", quotaRequestBody(t, AccountQuotaRequest{})},
		{"not twelve digits", quotaRequestBody(t, AccountQuotaRequest{AccountID: "42"})},
		{"unknown account", quotaRequestBody(t, AccountQuotaRequest{AccountID: "000000000099"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := gw.adminGetAccountQuota(t.Context(), tt.body)
			require.Error(t, err)
			require.Equal(t, awserrors.ErrorInvalidRequest, err.Error())

			_, err = gw.adminPutAccountQuota(t.Context(), tt.body)
			require.Error(t, err)
			require.Equal(t, awserrors.ErrorInvalidRequest, err.Error())
		})
	}
}

// A gateway with quotas disabled has no override bucket; the admin surface must
// report that rather than pretend the write landed.
func TestAdminAccountQuotaWithoutQuotaService(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: &quotaIAMService{known: "000000000042"}}

	_, err := gw.adminGetAccountQuota(t.Context(), quotaRequestBody(t, AccountQuotaRequest{AccountID: "000000000042"}))
	require.Error(t, err)
	require.Equal(t, awserrors.ErrorInternalError, err.Error())
}

// Every dimension must appear in the response, and each must be labelled with
// the layer it came from: a limit with no provenance cannot be debugged.
func TestAccountQuotaResponseReportsSource(t *testing.T) {
	limits := handlers_quota.Limits{
		Enabled: true, VCPUs: 32, VPCs: 4, Subnets: 16, EIPs: 4,
		Volumes: 16, VolumesGiB: 200, RDSInstances: 2, LoadBalancers: 2,
	}
	over := handlers_quota.Overrides{VCPUs: quotaPtr(32)}

	resp := accountQuotaResponse("000000000002", over, limits)

	require.Equal(t, "000000000002", resp.AccountID)
	require.Len(t, resp.Limits, 8, "every dimension must be reported")
	require.Len(t, resp.Source, 8)

	require.Equal(t, 32, resp.Limits["vcpus"])
	require.Equal(t, sourceOverride, resp.Source["vcpus"])
	for _, inherited := range []string{"vpcs", "subnets", "eips", "volumes", "volumes_gib", "rds_instances", "load_balancers"} {
		require.Equal(t, sourceConfig, resp.Source[inherited], "%s must report as inherited", inherited)
	}
}

// An explicit zero is an override, not an absent field, so it must be reported
// as one rather than looking like an inherited limit that happens to be zero.
func TestAccountQuotaResponseExplicitZeroIsAnOverride(t *testing.T) {
	resp := accountQuotaResponse("000000000002",
		handlers_quota.Overrides{VPCs: quotaPtr(0)},
		handlers_quota.Limits{VPCs: 0, VCPUs: 16})

	require.Equal(t, 0, resp.Limits["vpcs"])
	require.Equal(t, sourceOverride, resp.Source["vpcs"])
	require.Equal(t, sourceConfig, resp.Source["vcpus"])
}

// The two quota methods must be routable, or they return InvalidAction on a
// cluster that otherwise looks correctly configured.
func TestAccountQuotaMethodsAreRegistered(t *testing.T) {
	require.True(t, adminMethods["GetAccountQuota"])
	require.True(t, adminMethods["PutAccountQuota"])
	require.Contains(t, AdminMethodNames(), "GetAccountQuota")
	require.Contains(t, AdminMethodNames(), "PutAccountQuota")
}
