// Package ec2quota adapts the account quota service to the capability the EC2
// actions consume.
package ec2quota

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
	admissionquota "github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	"github.com/nats-io/nats.go"
)

// Adapter builds the NATS-backed instance-type resolver the quota service's
// retype gate takes, and narrows the launch charge to the reservation type.
type Adapter struct{ *admissionquota.Service }

func (q Adapter) EnforceRetype(ctx context.Context, nc *nats.Conn, nodeIDs func() []string, accountID, instanceID, newType string) (int, error) {
	return q.Service.EnforceRetype(ctx, admissionquota.NATSInstanceTypeResolver(nc, nodeIDs), accountID, instanceID, newType)
}

func (q Adapter) ChargeLaunch(ctx context.Context, accountID string, reservation *ec2.Reservation) error {
	return q.Service.ChargeLaunch(ctx, accountID, reservation)
}
