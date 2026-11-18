package routetable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/prefixlist"
	ec2vpcendpoint "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpcendpoint"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// EndpointLookup is the narrow endpoint query route tables need: which tables
// carry an effective gateway endpoint route, and to which endpoint.
type EndpointLookup interface {
	EffectiveEndpoints(ctx context.Context, rtbIDs []string) (map[string]string, error)
}

var (
	_ EndpointLookup             = (*ec2vpcendpoint.Store)(nil)
	_ ec2vpcendpoint.TableLookup = (*RouteTableServiceImpl)(nil)
)

// SetEndpointLookup installs the endpoint query. Called by the daemon after the
// endpoint service is constructed.
func (s *RouteTableServiceImpl) SetEndpointLookup(e EndpointLookup) { s.endpoints = e }

// RouteTableVpc returns the VPC of accountID's table rtbID, or "" when the
// table does not exist.
func (s *RouteTableServiceImpl) RouteTableVpc(ctx context.Context, accountID, rtbID string) (string, error) {
	entry, err := s.rtbKV.Get(ctx, kvstore.AccountKey(accountID, rtbID))
	if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrInvalidKey) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read route table %s: %w", rtbID, err)
	}
	var record RouteTableRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return "", fmt.Errorf("decode route table %s: %w", rtbID, err)
	}
	return record.VpcId, nil
}

func (s *RouteTableServiceImpl) s3PrefixList() string { return prefixlist.ID(s.config.Region) }

// deriveEndpointRoutes appends each table's effective endpoint route to its
// in-memory record. The route is never stored, so it cannot outlive the endpoint.
func (s *RouteTableServiceImpl) deriveEndpointRoutes(ctx context.Context, records []*RouteTableRecord) error {
	if s.endpoints == nil || len(records) == 0 {
		return nil
	}
	ids := make([]string, len(records))
	for i, rec := range records {
		ids[i] = rec.RouteTableId
	}
	routed, err := s.endpoints.EffectiveEndpoints(ctx, ids)
	if err != nil {
		return err
	}
	pl := s.s3PrefixList()
	for _, rec := range records {
		if vpce, ok := routed[rec.RouteTableId]; ok {
			rec.Routes = append(rec.Routes, RouteRecord{
				DestinationPrefixListId: pl, GatewayId: vpce, Origin: ec2.RouteOriginCreateRoute, State: ec2.RouteStateActive,
			})
		}
	}
	return nil
}

// endpointRouteWrite answers a create or replace naming the S3 list as AWS
// does: an endpoint route is the endpoint's to write, never a route's.
func (s *RouteTableServiceImpl) endpointRouteWrite(prefixListID, gatewayID, vpceID *string) error {
	if pl := aws.StringValue(prefixListID); pl == "" || pl != s.s3PrefixList() {
		return nil
	}
	if aws.StringValue(vpceID) != "" {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "Cannot create or replace a prefix list route targeting a VPC Endpoint.")
	}
	if gw := aws.StringValue(gatewayID); strings.HasPrefix(gw, "vpce-") {
		return gatewayNotFoundError(gw)
	}
	return nil
}

// endpointRouteDelete refuses deleting the derived route, which only modifying
// or deleting the endpoint removes.
func (s *RouteTableServiceImpl) endpointRouteDelete(ctx context.Context, rtbID string) error {
	pl := s.s3PrefixList()
	if s.endpoints != nil {
		routed, err := s.endpoints.EffectiveEndpoints(ctx, []string{rtbID})
		if err != nil {
			slog.ErrorContext(ctx, "DeleteRoute: endpoint route lookup failed", "routeTableId", rtbID, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		if vpce, ok := routed[rtbID]; ok {
			return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "cannot remove VPC endpoint route %s in route table %s", vpce, rtbID)
		}
	}
	return awserrors.Errorf(awserrors.ErrorInvalidRouteNotFound, "no route with destination-prefix-list-id %s in route table %s", pl, rtbID)
}
