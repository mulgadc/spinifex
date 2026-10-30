package registryadapt

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	ecrregistry "github.com/mulgadc/spinifex/spinifex/domains/ecr/registry"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	ingresshttp "github.com/mulgadc/spinifex/spinifex/ingress/http"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubRehydrator struct {
	p credential.Principal
	f *credential.Failure
}

func (s stubRehydrator) Rehydrate(_, _ string) (credential.Principal, *credential.Failure) {
	return s.p, s.f
}

func TestRehydrate_FailureCodeMapping(t *testing.T) {
	cases := []struct {
		code        string
		unavailable bool
	}{
		{awserrors.ErrorInternalError, true},
		{awserrors.ErrorInvalidClientTokenId, false},
		{awserrors.ErrorExpiredToken, false},
		{awserrors.ErrorAccessDenied, false},
		{"SomethingUnforeseen", false},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			_, err := registryRehydrator{stubRehydrator{f: &credential.Failure{Code: c.code}}}.Rehydrate("AKIA", "000000000001")
			require.Error(t, err)
			assert.Equal(t, c.unavailable, errors.Is(err, ecrregistry.ErrUnavailable))
			assert.Equal(t, !c.unavailable, errors.Is(err, ecrregistry.ErrInvalidCredential))
		})
	}
}

type stubRoleRehydrator struct {
	p credential.Principal
	f *credential.Failure
}

func (s stubRoleRehydrator) RehydrateRole(_, _, _ string) (credential.Principal, *credential.Failure) {
	return s.p, s.f
}

func TestRehydrateRole_FailureMapping(t *testing.T) {
	cases := []struct {
		name string
		f    *credential.Failure
		is   error
		isnt []error
	}{
		{"role gone", &credential.Failure{Code: awserrors.ErrorInvalidClientTokenId, RoleGone: true},
			ecrregistry.ErrRoleGone, []error{ecrregistry.ErrUnavailable, ecrregistry.ErrInvalidCredential}},
		{"dependency fault", &credential.Failure{Code: awserrors.ErrorInternalError},
			ecrregistry.ErrUnavailable, []error{ecrregistry.ErrRoleGone}},
		{"other invalid", &credential.Failure{Code: awserrors.ErrorInvalidClientTokenId},
			ecrregistry.ErrInvalidCredential, []error{ecrregistry.ErrRoleGone, ecrregistry.ErrUnavailable}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := registryRoleRehydrator{stubRoleRehydrator{f: c.f}}.RehydrateRole("000000000001", "AROA", "arn")
			require.Error(t, err)
			assert.ErrorIs(t, err, c.is)
			for _, no := range c.isnt {
				assert.NotErrorIs(t, err, no)
			}
		})
	}
}

func TestRehydrateRole_ProjectsThePrincipal(t *testing.T) {
	in := credential.Principal{
		Identity: "session", AccountID: "000000000001", PrincipalType: "assumed-role",
		AssumedRoleARN: "ar", AssumedRoleID: "ari", UnderlyingRoleARN: "ur", UserID: "uid", CallerARN: "carn",
	}
	got, err := registryRoleRehydrator{stubRoleRehydrator{p: in}}.RehydrateRole("000000000001", "AROA", "ar")
	require.NoError(t, err)
	assert.Equal(t, ecrregistry.Principal(in), got)
}

type stubCredentials struct {
	stubRehydrator
	stubRoleRehydrator
}

func TestConfig_WiresBothRehydrators(t *testing.T) {
	cfg := Config(ecrregistry.BridgeConfig{}, stubCredentials{}, nil)
	assert.NotNil(t, cfg.Rehydrator)
	assert.NotNil(t, cfg.RoleRehydrator)
}

func TestRehydrate_ProjectsEveryPrincipalField(t *testing.T) {
	in := credential.Principal{
		Identity: "dev", AccountID: "000000000001", PrincipalType: "user",
		AssumedRoleARN: "ar", AssumedRoleID: "ari", UnderlyingRoleARN: "ur",
		UserID: "uid", UserARN: "uarn", CallerARN: "carn",
	}
	got, err := registryRehydrator{stubRehydrator{p: in}}.Rehydrate("AKIA", "000000000001")
	require.NoError(t, err)
	assert.Equal(t, ecrregistry.Principal(in), got)
}

func TestTarget_ReadsHostMatcherContext(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	_, _, ok := registryTarget(r)
	assert.False(t, ok, "no matched host means no target")

	r = r.WithContext(ingresshttp.WithTarget(r.Context(), "000000000001", "ap-southeast-2"))
	account, region, ok := registryTarget(r)
	assert.True(t, ok)
	assert.Equal(t, "000000000001", account)
	assert.Equal(t, "ap-southeast-2", region)
}

func TestAuthorizeFailure(t *testing.T) {
	assert.NoError(t, authorizeFailure(nil))

	denied := errors.New(awserrors.ErrorAccessDenied)
	got := authorizeFailure(denied)
	assert.Equal(t, denied, got)
	assert.NotErrorIs(t, got, ecrregistry.ErrUnavailable)

	outage := authorizeFailure(errors.New(awserrors.ErrorInternalError))
	assert.ErrorIs(t, outage, ecrregistry.ErrUnavailable)
}
