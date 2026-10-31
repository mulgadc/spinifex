package gateway

import (
	"net/http"

	"github.com/mulgadc/bluebottle/pkg/auth"
	"github.com/mulgadc/spinifex/spinifex/domains/ecs/awsapi"
	eksawsapi "github.com/mulgadc/spinifex/spinifex/domains/eks/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/passrole"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// mockRoleResolver resolves roles through the gateway's mock IAM service, the
// way the IAM-owned resolver does against the real store.
type mockRoleResolver struct{ iam IAMServiceLike }

// IAMServiceLike is the slice of the IAM service the mock resolver reads.
type IAMServiceLike interface {
	CanonicalResourceARN(accountID string, kind arn.IAMResourceType, name string) (string, error)
}

func (m mockRoleResolver) ResolveRole(accountID, roleARN string) (passrole.Role, error) {
	roleAccount, name, err := auth.ParseRoleARN(roleARN)
	if err != nil {
		return passrole.Role{}, passrole.ErrMalformed
	}
	if roleAccount != accountID {
		return passrole.Role{}, passrole.ErrForeignAccount
	}
	canonical, err := m.iam.CanonicalResourceARN(roleAccount, arn.IAMRole, name)
	if err != nil {
		if awserrors.IsErrorCode(err, awserrors.ErrorIAMNoSuchEntity) {
			return passrole.Role{}, passrole.ErrNotFound
		}
		return passrole.Role{}, err
	}
	if canonical != roleARN {
		return passrole.Role{}, passrole.ErrNotFound
	}
	return passrole.Role{ARN: canonical, Name: name}, nil
}

var _ passrole.Resolver = mockRoleResolver{}

// withContainers registers ECS and EKS on gw with the NATS connection and
// mock IAM service it holds, as the awsgw role composes them.
func withContainers(gw *GatewayConfig) *GatewayConfig {
	b := dispatch.NewBuilder()
	if err := b.Register(awsapi.NewRegistration(awsapi.Deps{NATS: gw.NATSConn})); err != nil {
		panic(err)
	}
	roles, _ := gw.IAMService.(IAMServiceLike)
	if err := b.Register(eksawsapi.NewRegistration(eksawsapi.Deps{NATS: gw.NATSConn, Roles: mockRoleResolver{iam: roles}})); err != nil {
		panic(err)
	}
	gw.Services = b.Build()
	return gw
}

func (gw *GatewayConfig) serveRegistered(service string, w http.ResponseWriter, r *http.Request) error {
	if gw.Services == nil {
		withContainers(gw)
	}
	entry, ok := gw.registered(service)
	if !ok {
		panic(service + " is not registered on this gateway")
	}
	return gw.dispatchRegistered(entry, w, r)
}

func (gw *GatewayConfig) serveECS(w http.ResponseWriter, r *http.Request) error {
	return gw.serveRegistered(awsapi.ServiceName, w, r)
}

func (gw *GatewayConfig) serveEKS(w http.ResponseWriter, r *http.Request) error {
	return gw.serveRegistered(eksawsapi.ServiceName, w, r)
}
