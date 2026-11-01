package awsgw

import (
	"github.com/go-chi/chi/v5"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/gateway"
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
	Debug         bool
	Region        string
	NATS          *nats.Conn
	ExpectedNodes int
	IAM           iamdomain.IAMService
	STS           stsdomain.STSService
	Credentials   credential.Verifier
	Admission     pipeline.AccountAdmission
	PublicRoutes  func(chi.Router)
	PreAuth       []dispatch.PreAuth
}

// composeGateway is the single place the gateway's hooks are set for
// production; a hook left unset fails requests closed or drops a route.
func composeGateway(in gatewayInputs, regs ...dispatch.Registration) (gateway.GatewayConfig, error) {
	gw := gateway.GatewayConfig{
		Debug:          in.Debug,
		DisableLogging: false,
		NATSConn:       in.NATS,
		ExpectedNodes:  in.ExpectedNodes,
		Region:         in.Region,
		IAMService:     in.IAM,
		STSService:     in.STS,
		Credentials:    in.Credentials,
		Admission:      in.Admission,
		Policy:         policyauth.New(in.IAM),
		Readiness:      clusterready.New(in.NATS),
		Admin:          gateway.AdminSurface{Service: operatoradmin.ServiceName, WithRequestID: operatoradmin.WithRequestID},
		PublicRoutes:   in.PublicRoutes,
		PreAuth:        in.PreAuth,
	}
	if err := wireServiceRegistry(&gw, regs...); err != nil {
		return gateway.GatewayConfig{}, err
	}
	return gw, nil
}
