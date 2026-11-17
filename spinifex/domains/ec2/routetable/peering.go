package routetable

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/service/ec2"

	ec2vpcpeering "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpcpeering"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// PeeringLookup is the narrow peering query route tables need: the connection
// a route may target, and which connections are active for a route's state.
type PeeringLookup interface {
	RouteTarget(ctx context.Context, accountID, id string) (*ec2vpcpeering.RouteTarget, error)
	ActiveConnections(ctx context.Context, ids []string) (map[string]bool, error)
}

var _ PeeringLookup = (*ec2vpcpeering.Store)(nil)

// SetPeeringLookup installs the peering query. Called by the daemon after the
// peering service is constructed.
func (s *RouteTableServiceImpl) SetPeeringLookup(p PeeringLookup) {
	s.peering = p
}

func peeringNotFoundError(id string) error {
	return awserrors.IDNotFound(awserrors.ErrorInvalidVpcPeeringConnectionIDNotFound, "vpcPeeringConnection", id)
}

// peeringRoute checks that pcxID is a connection the caller can route to from
// record's VPC. Its state is derived when described, so none is stored.
func (s *RouteTableServiceImpl) peeringRoute(ctx context.Context, accountID string, record *RouteTableRecord, destCidr, pcxID string) (RouteRecord, error) {
	if s.peering == nil {
		slog.ErrorContext(ctx, "Route to a VPC peering connection with no peering lookup installed", "vpcPeeringConnectionId", pcxID)
		return RouteRecord{}, errors.New(awserrors.ErrorServerInternal)
	}
	target, err := s.peering.RouteTarget(ctx, accountID, pcxID)
	if err != nil {
		slog.ErrorContext(ctx, "VPC peering route target lookup failed", "vpcPeeringConnectionId", pcxID, "err", err)
		return RouteRecord{}, errors.New(awserrors.ErrorServerInternal)
	}
	if target == nil {
		return RouteRecord{}, peeringNotFoundError(pcxID)
	}
	if record.VpcId != target.RequesterVpcID && record.VpcId != target.AccepterVpcID {
		return RouteRecord{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"route table %s and network gateway %s belong to different networks", record.RouteTableId, pcxID)
	}
	return RouteRecord{DestinationCidrBlock: destCidr, VpcPeeringConnectionId: pcxID, Origin: "CreateRoute"}, nil
}

// derivePeeringStates sets each peering route's state on records in memory:
// active while its connection is active, blackhole otherwise.
func (s *RouteTableServiceImpl) derivePeeringStates(ctx context.Context, records []*RouteTableRecord) error {
	var ids []string
	for _, rec := range records {
		for _, r := range rec.Routes {
			if r.VpcPeeringConnectionId != "" {
				ids = append(ids, r.VpcPeeringConnectionId)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	active := map[string]bool{}
	if s.peering != nil {
		var err error
		if active, err = s.peering.ActiveConnections(ctx, ids); err != nil {
			return err
		}
	}
	for _, rec := range records {
		for i := range rec.Routes {
			if id := rec.Routes[i].VpcPeeringConnectionId; id != "" {
				rec.Routes[i].State = ec2.RouteStateBlackhole
				if active[id] {
					rec.Routes[i].State = ec2.RouteStateActive
				}
			}
		}
	}
	return nil
}
