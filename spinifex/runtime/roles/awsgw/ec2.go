package awsgw

import (
	"context"
	"net"

	"github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	ec2awsapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi"
	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/ec2quota"
	"github.com/nats-io/nats.go"
)

// ec2Wiring is what the gateway role holds that the EC2 control plane needs.
type ec2Wiring struct {
	NATS           *nats.Conn
	IAM            ec2instanceapi.InstanceProfiles
	Quota          *quota.Service
	ActiveNodes    func(context.Context) int
	ExpectedNodes  int
	NodeIDs        []string
	AZ             string
	RegistryHost   string
	RegistryPort   string
	InstanceStatus ec2instanceapi.StatusSynthesis
}

// newEC2Registration composes the EC2 control-plane registration from w.
func newEC2Registration(w ec2Wiring) dispatch.Registration {
	return ec2awsapi.NewRegistration(ec2awsapi.Deps{
		NATS:           w.NATS,
		IAM:            w.IAM,
		Quota:          ec2quota.Adapter{Service: w.Quota},
		ActiveNodes:    w.ActiveNodes,
		ExpectedNodes:  w.ExpectedNodes,
		NodeIDs:        w.NodeIDs,
		AZ:             w.AZ,
		Endpoint:       advertisedEndpoint(w.RegistryHost, w.RegistryPort),
		InstanceStatus: w.InstanceStatus,
	})
}

// advertisedEndpoint returns this gateway's own dialable base URL, from the
// same registry host and port that already advertise ECR. Falls back to
// localhost only when no concrete host is configured.
func advertisedEndpoint(host, port string) string {
	if host == "" {
		host = "localhost"
	}
	if port == "" {
		port = "9999"
	}
	if port == "443" {
		return "https://" + host
	}
	return "https://" + net.JoinHostPort(host, port)
}
