//test:in-package — exercises IAM_Request with policy and auth context helpers.

package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type iamAuthzPolicyService struct {
	policyMockIAMService

	arns          map[string]string
	canonicalErr  error
	deleteRoleErr error
	deleted       []string
}

func (s *iamAuthzPolicyService) CanonicalResourceARN(_ string, _ arn.IAMResourceType, name string) (string, error) {
	if s.canonicalErr != nil {
		return "", s.canonicalErr
	}
	resource, ok := s.arns[name]
	if !ok {
		return "", errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return resource, nil
}

func (s *iamAuthzPolicyService) DeleteRole(_ string, input *iam.DeleteRoleInput) (*iam.DeleteRoleOutput, error) {
	s.deleted = append(s.deleted, *input.RoleName)
	if s.deleteRoleErr != nil {
		return nil, s.deleteRoleErr
	}
	return &iam.DeleteRoleOutput{}, nil
}

func iamScopedGateway(resources map[string]string, statements ...handlers_iam.Statement) (*GatewayConfig, *iamAuthzPolicyService) {
	docs := []handlers_iam.PolicyDocument{{Version: "2012-10-17", Statement: statements}}
	svc := &iamAuthzPolicyService{
		policyMockIAMService: policyMockIAMService{
			getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) { return docs, nil },
			getRolePoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) { return docs, nil },
		},
		arns: resources,
	}
	return &GatewayConfig{DisableLogging: true, IAMService: svc}, svc
}

func dispatchIAM(t *testing.T, gw *GatewayConfig, body string) error {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx := context.WithValue(req.Context(), ctxService, "iam")
	ctx = context.WithValue(ctx, ctxAccountID, authzAccountID)
	req = withTestIdentity(req.WithContext(ctx))
	return gw.IAM_Request(httptest.NewRecorder(), req)
}

func TestIAMRequest_ScopedDenyFires(t *testing.T) {
	gw, svc := iamScopedGateway(map[string]string{
		"prod": "arn:aws:iam::123456789012:role/service-roles/prod",
		"dev":  "arn:aws:iam::123456789012:role/service-roles/dev",
	},
		statement("Allow", "iam:*", "*"),
		statement("Deny", "iam:DeleteRole", "arn:aws:iam::*:role/service-roles/prod"),
	)

	err := dispatchIAM(t, gw, "Action=DeleteRole&RoleName=prod")
	assertDenied(t, err)
	require.NoError(t, dispatchIAM(t, gw, "Action=DeleteRole&RoleName=dev"))
	assert.Equal(t, []string{"dev"}, svc.deleted)
}

func TestIAMRequest_ScopedAllowGrants(t *testing.T) {
	gw, svc := iamScopedGateway(map[string]string{
		"prod": "arn:aws:iam::123456789012:role/service-roles/prod",
		"dev":  "arn:aws:iam::123456789012:role/service-roles/dev",
	}, statement("Allow", "iam:DeleteRole", "arn:aws:iam::*:role/service-roles/dev"))

	require.NoError(t, dispatchIAM(t, gw, "Action=DeleteRole&RoleName=dev"))
	err := dispatchIAM(t, gw, "Action=DeleteRole&RoleName=prod")
	assertDenied(t, err)
	assert.Equal(t, []string{"dev"}, svc.deleted)
}

func TestIAMRequest_MissingTargetAndLookupFailure(t *testing.T) {
	t.Run("account-wide grant reaches handler no-such-entity", func(t *testing.T) {
		gw, svc := iamScopedGateway(nil, statement("Allow", "iam:*", "*"))
		svc.deleteRoleErr = errors.New(awserrors.ErrorIAMNoSuchEntity)
		err := dispatchIAM(t, gw, "Action=DeleteRole&RoleName=missing")
		require.EqualError(t, err, awserrors.ErrorIAMNoSuchEntity)
	})

	t.Run("scoped grant denies unresolved target", func(t *testing.T) {
		gw, _ := iamScopedGateway(nil,
			statement("Allow", "iam:DeleteRole", "arn:aws:iam::*:role/missing"))
		err := dispatchIAM(t, gw, "Action=DeleteRole&RoleName=missing")
		assertDenied(t, err)
	})

	t.Run("storage failure fails closed", func(t *testing.T) {
		gw, svc := iamScopedGateway(nil, statement("Allow", "iam:*", "*"))
		svc.canonicalErr = errors.New("storage unavailable")
		err := dispatchIAM(t, gw, "Action=DeleteRole&RoleName=app")
		require.EqualError(t, err, awserrors.ErrorInternalError)
		assert.Empty(t, svc.deleted)
	})
}

func TestIAMRequest_MissingIdentifierStaysValidationFault(t *testing.T) {
	gw, _ := iamScopedGateway(nil, statement("Allow", "iam:*", "*"))
	err := dispatchIAM(t, gw, "Action=DeleteRole")
	require.EqualError(t, err, awserrors.ErrorMissingParameter)
}
