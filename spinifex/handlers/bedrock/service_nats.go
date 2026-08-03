package handlers_bedrock

import (
	"context"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// defaultNATSTimeout bounds a request-reply round trip. Ensure's own slow
// work (VM launch + readiness probe) happens after the daemon has already
// replied STARTING, so this timeout only needs to cover the claim, not the
// launch.
const defaultNATSTimeout = 30 * time.Second

// NATSEndpointService is the NATS-forwarding EndpointService client: the
// gateway process (or any other caller off the daemon's own node) uses this
// instead of importing handlers_bedrock's JetStream/launch dependencies.
type NATSEndpointService struct {
	nc      *nats.Conn
	timeout time.Duration
}

var _ EndpointService = (*NATSEndpointService)(nil)

// NewNATSEndpointService constructs a client bound to nc.
func NewNATSEndpointService(nc *nats.Conn) *NATSEndpointService {
	return &NATSEndpointService{nc: nc, timeout: defaultNATSTimeout}
}

func (s *NATSEndpointService) Ensure(ctx context.Context, in *EnsureEndpointInput, accountID string) (*EnsureEndpointOutput, error) {
	return utils.NATSRequest[EnsureEndpointOutput](ctx, s.nc, SubjectEnsureEndpoint, in, s.timeout, accountID)
}

func (s *NATSEndpointService) Describe(ctx context.Context, in *DescribeEndpointInput, accountID string) (*DescribeEndpointOutput, error) {
	return utils.NATSRequest[DescribeEndpointOutput](ctx, s.nc, SubjectDescribeEndpoint, in, s.timeout, accountID)
}

func (s *NATSEndpointService) List(ctx context.Context, in *ListEndpointsInput, accountID string) (*ListEndpointsOutput, error) {
	return utils.NATSRequest[ListEndpointsOutput](ctx, s.nc, SubjectListEndpoints, in, s.timeout, accountID)
}

func (s *NATSEndpointService) Delete(ctx context.Context, in *DeleteEndpointInput, accountID string) (*DeleteEndpointOutput, error) {
	return utils.NATSRequest[DeleteEndpointOutput](ctx, s.nc, SubjectDeleteEndpoint, in, s.timeout, accountID)
}
