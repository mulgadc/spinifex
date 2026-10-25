//test:in-package — exercises IAM_Request with policy and auth context helpers.

package gateway

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/service/iam"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func addRoleGateway(statements ...iamdomain.Statement) (*GatewayConfig, *addRoleIAMService) {
	base, inner := iamScopedGateway(map[string]string{
		"web":   passRoleProfileARN,
		"admin": passRoleAdminARN,
	}, statements...)
	svc := &addRoleIAMService{iamAuthzPolicyService: *inner}
	base.IAMService = svc
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
		passRoleTo("Allow", "StringEquals", ec2ServicePrincipal),
	)
	require.NoError(t, dispatchIAM(t, ec2Grant, addAdminToWeb))
	assert.Equal(t, []string{"admin"}, svc.added)

	ecsGrant, _ := addRoleGateway(
		statement("Allow", "iam:AddRoleToInstanceProfile", "*"),
		passRoleTo("Allow", "StringEquals", ecsTasksServicePrincipal),
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
