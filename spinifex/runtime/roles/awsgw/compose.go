package awsgw

import (
	"github.com/go-chi/chi/v5"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/clusterready"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/policyauth"
	"github.com/nats-io/nats.go"

	operatoradmin "github.com/mulgadc/spinifex/spinifex/operator/admin"
)

// gatewayInputs are the collaborators launchService hands to composeGateway.
type gatewayInputs struct {
	Debug        bool
	Region       string
	NATS         *nats.Conn
	IAM          iamdomain.IAMService
	Credentials  credential.Verifier
	Admission    pipeline.AccountAdmission
	PublicRoutes func(chi.Router)
	PreAuth      []dispatch.PreAuth
}

// composeGateway is the single place the gateway's hooks are set for
// production; a hook left unset fails requests closed or drops a route.
func composeGateway(in gatewayInputs, regs ...dispatch.Registration) (pipeline.GatewayConfig, error) {
	gw := pipeline.GatewayConfig{
		Debug:          in.Debug,
		DisableLogging: false,
		Region:         in.Region,
		Credentials:    in.Credentials,
		Admission:      in.Admission,
		Policy:         policyauth.New(in.IAM),
		Readiness:      clusterready.New(in.NATS),
		Admin:          pipeline.AdminSurface{Service: operatoradmin.ServiceName, WithRequestID: operatoradmin.WithRequestID},
		PublicRoutes:   in.PublicRoutes,
		PreAuth:        in.PreAuth,
	}
	if err := wireServiceRegistry(&gw, regs...); err != nil {
		return pipeline.GatewayConfig{}, err
	}
	return gw, nil
}
