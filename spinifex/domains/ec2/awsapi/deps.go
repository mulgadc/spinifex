package awsapi

import (
	"context"
	"sync"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Quota is the account quota the EC2 actions consult and charge. It is declared
// here because domains/admission/quota imports the EC2 action packages to count
// live resources, and its retype gate takes a resolver type this package must
// not name.
type Quota interface {
	EnforceLaunch(ctx context.Context, accountID, instanceType string, maxCount int) error
	ChargeLaunch(ctx context.Context, accountID string, reservation *ec2.Reservation) error
	// EnforceRetype returns the vCPU delta AddVCPU charges once the retype applies.
	EnforceRetype(ctx context.Context, nc *nats.Conn, nodeIDs func() []string, accountID, instanceID, newType string) (int, error)
	AddVCPU(ctx context.Context, accountID string, delta int) error
	EnforceVolumeCreate(ctx context.Context, nc *nats.Conn, accountID string, requestedGiB int) error
	EnforceVolumeModify(ctx context.Context, nc *nats.Conn, accountID, volumeID string, newGiB int) error
	EnforceEIPs(ctx context.Context, nc *nats.Conn, accountID string, want int) error
	EnforceSubnets(ctx context.Context, nc *nats.Conn, accountID string, want int) error
	EnforceVPCs(ctx context.Context, nc *nats.Conn, accountID string, want int) error
}

// Deps are the composed capabilities the EC2 control plane dispatches to.
type Deps struct {
	NATS  *nats.Conn
	IAM   ec2instanceapi.InstanceProfiles
	Quota Quota
	// ActiveNodes is the number of daemons currently answering.
	ActiveNodes   func(ctx context.Context) int
	ExpectedNodes int
	NodeIDs       []string
	AZ            string
	// Endpoint is the gateway's own dialable base URL DescribeRegions advertises.
	Endpoint       string
	InstanceStatus ec2instanceapi.StatusSynthesis
}

// Handler serves EC2 requests over its Deps. It binds the client-token store on
// first use and is safe for concurrent requests.
type Handler struct {
	Deps

	tokenOnce sync.Once
	tokenKV   jetstream.KeyValue
	tokenErr  error
}

// NewHandler returns the handler over deps.
func NewHandler(deps Deps) *Handler { return &Handler{Deps: deps} }

// call is one request's view of the handler: the handler's capabilities plus the
// region and the PassRole gate of the authenticated caller.
type call struct {
	*Handler

	Region   string
	passRole func(roleARN string) error
}
