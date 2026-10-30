package gateway

import (
	"context"
	"net"
	"net/http"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	ec2awsapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go"
)

// ec2Quota adapts the account quota service to the capability the EC2 actions
// consume, building the NATS-backed instance-type resolver its retype gate takes.
type ec2Quota struct{ *quota.Service }

var _ ec2awsapi.Quota = ec2Quota{}

func (q ec2Quota) EnforceRetype(ctx context.Context, nc *nats.Conn, nodeIDs func() []string, accountID, instanceID, newType string) (int, error) {
	return q.Service.EnforceRetype(ctx, quota.NATSInstanceTypeResolver(nc, nodeIDs), accountID, instanceID, newType)
}

func (q ec2Quota) ChargeLaunch(ctx context.Context, accountID string, reservation *ec2.Reservation) error {
	return q.Service.ChargeLaunch(ctx, accountID, reservation)
}

// advertisedEndpoint returns this gateway's own dialable base URL, from the
// same RegistryHost/RegistryPort that already advertise ECR. Falls back to
// localhost only when no concrete host is configured.
func (gw *GatewayConfig) advertisedEndpoint() string {
	host := gw.RegistryHost
	if host == "" {
		host = "localhost"
	}
	port := gw.RegistryPort
	if port == "" {
		port = "9999"
	}
	if port == "443" {
		return "https://" + host
	}
	return "https://" + net.JoinHostPort(host, port)
}

// ec2Deps composes the capabilities the EC2 actions dispatch to from the
// gateway's own configuration.
func (gw *GatewayConfig) ec2Deps() ec2awsapi.Deps {
	return ec2awsapi.Deps{
		NATS:           gw.NATSConn,
		IAM:            gw.IAMService,
		Quota:          ec2Quota{gw.Quota},
		ActiveNodes:    gw.DiscoverActiveNodes,
		ExpectedNodes:  gw.ExpectedNodes,
		NodeIDs:        gw.NodeIDs,
		AZ:             gw.AZ,
		Endpoint:       gw.advertisedEndpoint(),
		InstanceStatus: gw.InstanceStatus,
	}
}

// EC2_Request serves one authenticated EC2 request.
func (gw *GatewayConfig) EC2_Request(w http.ResponseWriter, r *http.Request) error {
	gw.ec2Once.Do(func() { gw.ec2Handler = ec2awsapi.NewHandler(gw.ec2Deps()) })
	return gw.ec2Handler.Dispatch(w, dispatch.Invocation{
		Request:   r,
		AccountID: callerPrincipalOf(r).accountID,
		Service:   ec2awsapi.ServiceName,
		Region:    gw.Region,
		Authorize: func(service, action string, resources []string, keys iampolicy.ConditionKeys) error {
			return gw.checkPolicyResourcesWithKeys(r, service, action, resources, keys)
		},
	})
}
