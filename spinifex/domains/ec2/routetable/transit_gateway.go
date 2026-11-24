package routetable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/service/ec2"

	ec2tgw "github.com/mulgadc/spinifex/spinifex/domains/ec2/tgw"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// TransitGatewayLookup is the narrow Transit Gateway query route tables need:
// whether a VPC may route to a gateway, and whether such a route forwards.
type TransitGatewayLookup interface {
	RouteTarget(ctx context.Context, accountID, tgwID, vpcID string) (bool, error)
	RouteActive(ctx context.Context, accountID, tgwID, vpcID string) (bool, error)
}

var _ TransitGatewayLookup = (*ec2tgw.Store)(nil)

// Admission admits a write at a feature level. The caller calls done exactly
// once, after the write is recorded or abandoned.
type Admission func(ctx context.Context) (done func(), ok bool, err error)

// SetTransitGatewayLookup installs the Transit Gateway query and the feature
// level admission for routes that name a Transit Gateway.
func (s *RouteTableServiceImpl) SetTransitGatewayLookup(l TransitGatewayLookup, admit Admission) {
	s.tgws = l
	s.tgwAdmit = admit
}

func transitGatewayNotFoundError(id string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidTransitGatewayIDNotFound, "The transitGateway ID '%s' does not exist.", id)
}

// transitGatewayRoute checks that the route table's VPC may route to tgwID.
// The caller holds done until the route is written or refused.
func (s *RouteTableServiceImpl) transitGatewayRoute(ctx context.Context, accountID string, record *RouteTableRecord, destCidr, tgwID string) (RouteRecord, func(), error) {
	noop := func() {}
	if s.tgwAdmit == nil || s.tgws == nil {
		return RouteRecord{}, noop, transitGatewaysNotOffered()
	}
	done, ok, err := s.tgwAdmit(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "Transit Gateway route admission failed", "transitGatewayId", tgwID, "err", err)
		return RouteRecord{}, done, errors.New(awserrors.ErrorServerInternal)
	}
	if !ok {
		return RouteRecord{}, done, transitGatewaysNotOffered()
	}
	routable, err := s.tgws.RouteTarget(ctx, accountID, tgwID, record.VpcId)
	if err != nil {
		slog.ErrorContext(ctx, "Transit Gateway route target lookup failed", "transitGatewayId", tgwID, "err", err)
		return RouteRecord{}, done, errors.New(awserrors.ErrorServerInternal)
	}
	if !routable {
		return RouteRecord{}, done, transitGatewayNotFoundError(tgwID)
	}
	return RouteRecord{DestinationCidrBlock: destCidr, TransitGatewayId: tgwID, Origin: ec2.RouteOriginCreateRoute}, done, nil
}

func transitGatewaysNotOffered() error {
	return awserrors.Errorf(awserrors.ErrorUnsupportedOperation,
		"Transit Gateways are not available until every node in the formation runs a release that supports them.")
}

// deriveTransitGatewayStates sets each Transit Gateway route's state in
// memory: active while the gateway and the VPC's attachment are available.
func (s *RouteTableServiceImpl) deriveTransitGatewayStates(ctx context.Context, accountID string, records []*RouteTableRecord) error {
	for _, rec := range records {
		for i := range rec.Routes {
			id := rec.Routes[i].TransitGatewayId
			if id == "" {
				continue
			}
			active := false
			if s.tgws != nil {
				var err error
				if active, err = s.tgws.RouteActive(ctx, accountID, id, rec.VpcId); err != nil {
					return err
				}
			}
			rec.Routes[i].State = ec2.RouteStateBlackhole
			if active {
				rec.Routes[i].State = ec2.RouteStateActive
			}
		}
	}
	return nil
}

// CountTransitGatewayRoutes counts stored routes that name a Transit Gateway.
// An older daemon drops the field when it rewrites a table, so rollback waits
// for this to reach zero. It never creates the bucket.
func CountTransitGatewayRoutes(ctx context.Context, js jetstream.JetStream) (int, error) {
	kv, err := js.KeyValue(ctx, KVBucketRouteTables)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", KVBucketRouteTables, err)
	}
	keys, err := kvstore.Keys(ctx, kv)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return 0, fmt.Errorf("list %s: %w", KVBucketRouteTables, err)
	}
	n := 0
	for _, key := range keys {
		if key == kvstore.VersionKey {
			continue
		}
		entry, err := kv.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("read route table %s: %w", key, err)
		}
		var rec struct {
			Routes []struct {
				TransitGatewayId string `json:"transit_gateway_id"`
			} `json:"routes"`
		}
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			return 0, fmt.Errorf("decode route table %s: %w", key, err)
		}
		for _, r := range rec.Routes {
			if r.TransitGatewayId != "" {
				n++
			}
		}
	}
	return n, nil
}
