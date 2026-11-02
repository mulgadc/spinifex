package awsapi_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	. "github.com/mulgadc/spinifex/spinifex/domains/iam/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	authzAccountID           = gwtest.AccountID
	ec2ServicePrincipal      = "ec2.amazonaws.com"
	ecsTasksServicePrincipal = "ecs-tasks.amazonaws.com"
)

var statement = gwtest.Statement

type iamAuthzPolicyService struct {
	iamdomain.IAMService

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

// iamGateway pairs a caller holding statements with the service they act on.
type iamGateway struct {
	h   *gwtest.Harness
	svc iamdomain.IAMService
}

func iamScopedGateway(resources map[string]string, statements ...iamdomain.Statement) (*iamGateway, *iamAuthzPolicyService) {
	svc := &iamAuthzPolicyService{arns: resources}
	return &iamGateway{h: gwtest.Scoped(statements...), svc: svc}, svc
}

func dispatchIAM(t *testing.T, gw *iamGateway, body string) error {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return gw.h.Dispatch(NewRegistration(Deps{IAM: gw.svc}), ServiceName, httptest.NewRecorder(), req)
}

func assertDenied(t *testing.T, err error) {
	t.Helper()
	gwtest.AssertDenied(t, err)
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

const (
	passRoleProfileARN = "arn:aws:iam::" + authzAccountID + ":instance-profile/web"
	passRoleAdminARN   = "arn:aws:iam::" + authzAccountID + ":role/ops/admin"
	addAdminToWeb      = "Action=AddRoleToInstanceProfile&InstanceProfileName=web&RoleName=admin"
)

// addRoleIAMService records the roles the handler binds, so a test can prove a
// denied request never reached it.
type addRoleIAMService struct {
	iamAuthzPolicyService

	roleLookupErr error
	added         []string
}

func (s *addRoleIAMService) CanonicalResourceARN(accountID string, kind arn.IAMResourceType, name string) (string, error) {
	if kind == arn.IAMRole && s.roleLookupErr != nil {
		return "", s.roleLookupErr
	}
	return s.iamAuthzPolicyService.CanonicalResourceARN(accountID, kind, name)
}

func (s *addRoleIAMService) AddRoleToInstanceProfile(_ string, input *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	s.added = append(s.added, *input.RoleName)
	return &iam.AddRoleToInstanceProfileOutput{}, nil
}

func addRoleGateway(statements ...iamdomain.Statement) (*iamGateway, *addRoleIAMService) {
	base, inner := iamScopedGateway(map[string]string{
		"web":   passRoleProfileARN,
		"admin": passRoleAdminARN,
	}, statements...)
	svc := &addRoleIAMService{iamAuthzPolicyService: *inner}
	base.svc = svc
	return base, svc
}

// The escalation: a caller denied PassRole on a role binds it to a profile its
// instance already holds, and IMDS then serves the role's credentials.
func TestAddRoleToInstanceProfile_PassRoleDenyBlocksBinding(t *testing.T) {
	gw, svc := addRoleGateway(
		statement("Allow", "iam:AddRoleToInstanceProfile", "*"),
		statement("Allow", "iam:PassRole", "*"),
		statement("Deny", "iam:PassRole", passRoleAdminARN),
	)
	assertDenied(t, dispatchIAM(t, gw, addAdminToWeb))
	assert.Empty(t, svc.added)
}

func TestAddRoleToInstanceProfile_RequiresPassRoleGrant(t *testing.T) {
	gw, svc := addRoleGateway(statement("Allow", "iam:AddRoleToInstanceProfile", passRoleProfileARN))
	assertDenied(t, dispatchIAM(t, gw, addAdminToWeb))
	assert.Empty(t, svc.added)
}

// The grant is matched against the role's stored path, as the EC2 association
// check matches it, so a path-scoped grant behaves the same on both routes.
func TestAddRoleToInstanceProfile_PassRoleOnCanonicalPath(t *testing.T) {
	allowed, svc := addRoleGateway(
		statement("Allow", "iam:AddRoleToInstanceProfile", "*"),
		statement("Allow", "iam:PassRole", "arn:aws:iam::"+authzAccountID+":role/ops/*"),
	)
	require.NoError(t, dispatchIAM(t, allowed, addAdminToWeb))
	assert.Equal(t, []string{"admin"}, svc.added)

	wrongPath, _ := addRoleGateway(
		statement("Allow", "iam:AddRoleToInstanceProfile", "*"),
		statement("Allow", "iam:PassRole", "arn:aws:iam::"+authzAccountID+":role/admin"),
	)
	assertDenied(t, dispatchIAM(t, wrongPath, addAdminToWeb))
}

func TestAddRoleToInstanceProfile_PassedToServiceIsEC2(t *testing.T) {
	ec2Grant, svc := addRoleGateway(
		statement("Allow", "iam:AddRoleToInstanceProfile", "*"),
		gwtest.PassRoleTo("Allow", "StringEquals", ec2ServicePrincipal),
	)
	require.NoError(t, dispatchIAM(t, ec2Grant, addAdminToWeb))
	assert.Equal(t, []string{"admin"}, svc.added)

	ecsGrant, _ := addRoleGateway(
		statement("Allow", "iam:AddRoleToInstanceProfile", "*"),
		gwtest.PassRoleTo("Allow", "StringEquals", ecsTasksServicePrincipal),
	)
	assertDenied(t, dispatchIAM(t, ecsGrant, addAdminToWeb))
}

// An unknown role is reported only to a caller allowed to pass it, so the
// reply cannot be used to enumerate roles.
func TestAddRoleToInstanceProfile_UnknownRole(t *testing.T) {
	body := "Action=AddRoleToInstanceProfile&InstanceProfileName=web&RoleName=ghost"

	denied, svc := addRoleGateway(statement("Allow", "iam:AddRoleToInstanceProfile", "*"))
	assertDenied(t, dispatchIAM(t, denied, body))
	assert.Empty(t, svc.added)

	allowed, svc := addRoleGateway(
		statement("Allow", "iam:AddRoleToInstanceProfile", "*"),
		statement("Allow", "iam:PassRole", "arn:aws:iam::"+authzAccountID+":role/ghost"),
	)
	require.NoError(t, dispatchIAM(t, allowed, body))
	assert.Equal(t, []string{"ghost"}, svc.added)
}

func TestAddRoleToInstanceProfile_RoleLookupFaultFailsClosed(t *testing.T) {
	gw, svc := addRoleGateway(statement("Allow", "iam:*", "*"))
	svc.roleLookupErr = errors.New("storage unavailable")
	require.EqualError(t, dispatchIAM(t, gw, addAdminToWeb), awserrors.ErrorInternalError)
	assert.Empty(t, svc.added)
}

func TestAddRoleToInstanceProfile_MissingRoleStaysValidationFault(t *testing.T) {
	gw, _ := addRoleGateway(statement("Allow", "iam:AddRoleToInstanceProfile", "*"))
	err := dispatchIAM(t, gw, "Action=AddRoleToInstanceProfile&InstanceProfileName=web")
	require.EqualError(t, err, awserrors.ErrorMissingParameter)
}
