package gateway

import (
	"net/http"

	ec2awsapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/ec2quota"
)

// ec2Registration composes the EC2 registration from the NATS connection, IAM
// service and node count gw holds, as the awsgw role does. mutate adjusts the
// rest.
func ec2Registration(gw *GatewayConfig, mutate ...func(*ec2awsapi.Deps)) dispatch.Registration {
	deps := ec2awsapi.Deps{
		NATS:          gw.NATSConn,
		IAM:           gw.IAMService,
		Quota:         ec2quota.Adapter{Service: quotaFor(gw)},
		ActiveNodes:   gw.DiscoverActiveNodes,
		ExpectedNodes: gw.ExpectedNodes,
	}
	for _, m := range mutate {
		m(&deps)
	}
	return ec2awsapi.NewRegistration(deps)
}

// withEC2 registers EC2 alone on gw.
func withEC2(gw *GatewayConfig, mutate ...func(*ec2awsapi.Deps)) *GatewayConfig {
	return withServices(gw, ec2Registration(gw, mutate...))
}

// EC2_Request dispatches r to the registered EC2 service exactly as Request
// does past its NATS gate, registering it first from the fields gw holds when
// the test has not.
func (gw *GatewayConfig) EC2_Request(w http.ResponseWriter, r *http.Request) error {
	if _, ok := gw.Registered(ec2awsapi.ServiceName); !ok {
		withEC2(gw)
	}
	entry, _ := gw.Registered(ec2awsapi.ServiceName)
	return gw.dispatchRegistered(entry, w, r)
}

// withEC2Verifier is withVerifier for a test that signs as EC2: the service
// must be registered for the auth gate to serve its credential scope.
func withEC2Verifier(gw *GatewayConfig) *GatewayConfig {
	return withEC2(withVerifier(gw))
}

func ec2RegistrationInventory() map[string]dispatch.Inventory {
	return withEC2(&GatewayConfig{}).Services.Inventory()
}
