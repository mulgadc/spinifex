package handlers_ec2_routetable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	handlers_ec2_igw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/igw"
	handlers_ec2_natgw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/natgw"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Ensure RouteTableServiceImpl implements RouteTableService.
var _ RouteTableService = (*RouteTableServiceImpl)(nil)

// RouteTableServiceImpl implements Route Table operations with NATS JetStream persistence.
type RouteTableServiceImpl struct {
	config   *config.Config
	rtbKV    jetstream.KeyValue
	vpcKV    jetstream.KeyValue
	igwKV    jetstream.KeyValue
	subnetKV jetstream.KeyValue
	natgwKV  jetstream.KeyValue
	natsConn *nats.Conn
}

// NewRouteTableServiceImplWithNATS creates a Route Table service with NATS JetStream for persistence.
func NewRouteTableServiceImplWithNATS(ctx context.Context, cfg *config.Config, natsConn *nats.Conn) (*RouteTableServiceImpl, error) {
	js, err := jetstream.New(natsConn)
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	rtbKV, err := kvutil.GetOrCreateBucket(ctx, js, KVBucketRouteTables, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketRouteTables, err)
	}
	if err := migrate.DefaultRegistry.RunKV(ctx, KVBucketRouteTables, rtbKV, KVBucketRouteTablesVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketRouteTables, err)
	}

	vpcKV, err := kvutil.GetOrCreateBucket(ctx, js, handlers_ec2_vpc.KVBucketVPCs, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to get VPC KV bucket: %w", err)
	}

	igwKV, err := kvutil.GetOrCreateBucket(ctx, js, handlers_ec2_igw.KVBucketIGW, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to get IGW KV bucket: %w", err)
	}

	subnetKV, err := kvutil.GetOrCreateBucket(ctx, js, handlers_ec2_vpc.KVBucketSubnets, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to get subnet KV bucket: %w", err)
	}

	natgwKV, err := kvutil.GetOrCreateBucket(ctx, js, handlers_ec2_natgw.KVBucketNatGateways, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to get NAT Gateway KV bucket: %w", err)
	}

	slog.Info("RouteTable service initialized with JetStream KV", "bucket", KVBucketRouteTables)

	return &RouteTableServiceImpl{
		config:   cfg,
		rtbKV:    rtbKV,
		vpcKV:    vpcKV,
		igwKV:    igwKV,
		subnetKV: subnetKV,
		natgwKV:  natgwKV,
		natsConn: natsConn,
	}, nil
}

// getRouteTable retrieves a route table record from KV.
func (s *RouteTableServiceImpl) getRouteTable(ctx context.Context, accountID, rtbID string) (*RouteTableRecord, error) {
	entry, err := s.rtbKV.Get(ctx, utils.AccountKey(accountID, rtbID))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorInvalidRouteTableIDNotFound)
		}
		slog.ErrorContext(ctx, "Failed to read route table from KV", "routeTableId", rtbID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	var record RouteTableRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		slog.ErrorContext(ctx, "Corrupt route table record in KV", "routeTableId", rtbID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return &record, nil
}

// putRouteTable stores a route table record to KV.
func (s *RouteTableServiceImpl) putRouteTable(ctx context.Context, accountID string, record *RouteTableRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to marshal route table record", "routeTableId", record.RouteTableId, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.rtbKV.Put(ctx, utils.AccountKey(accountID, record.RouteTableId), data); err != nil {
		slog.ErrorContext(ctx, "Failed to write route table to KV", "routeTableId", record.RouteTableId, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	return nil
}

// rtbCASMaxRetries bounds optimistic-concurrency retries when a concurrent
// writer wins the CAS race on a route table record. High enough to absorb the
// parallel AssociateRouteTable calls Terraform fans out per route table.
const rtbCASMaxRetries = 16

// mutateRouteTableCAS applies mutate to a route table under optimistic
// concurrency: read with revision, mutate, then Update guarded by that
// revision, retrying when a concurrent writer wins. A blind read-modify-Put
// loses updates when callers associate several subnets with one route table at
// once. mutate reports whether it changed the record; a false return commits
// nothing.
func (s *RouteTableServiceImpl) mutateRouteTableCAS(ctx context.Context, accountID, rtbID string, mutate func(*RouteTableRecord) (bool, error)) error {
	key := utils.AccountKey(accountID, rtbID)
	for range rtbCASMaxRetries {
		entry, err := s.rtbKV.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				return errors.New(awserrors.ErrorInvalidRouteTableIDNotFound)
			}
			slog.ErrorContext(ctx, "Failed to read route table from KV", "routeTableId", rtbID, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}

		var record RouteTableRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.ErrorContext(ctx, "Corrupt route table record in KV", "routeTableId", rtbID, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}

		changed, err := mutate(&record)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}

		data, err := json.Marshal(&record)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to marshal route table record", "routeTableId", rtbID, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		if _, err := s.rtbKV.Update(ctx, key, data, entry.Revision()); err != nil {
			if errors.Is(err, jetstream.ErrKeyExists) {
				continue // CAS conflict — another writer won, re-read and retry.
			}
			slog.ErrorContext(ctx, "Failed to write route table to KV", "routeTableId", rtbID, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		return nil
	}
	return errors.New(awserrors.ErrorServerInternal)
}

// getVPCCidr looks up a VPC's CIDR block from the VPC KV bucket.
func (s *RouteTableServiceImpl) getVPCCidr(ctx context.Context, accountID, vpcID string) (string, error) {
	entry, err := s.vpcKV.Get(ctx, utils.AccountKey(accountID, vpcID))
	if err != nil {
		return "", errors.New(awserrors.ErrorInvalidVpcIDNotFound)
	}
	var vpcRecord handlers_ec2_vpc.VPCRecord
	if err := json.Unmarshal(entry.Value(), &vpcRecord); err != nil {
		return "", errors.New(awserrors.ErrorServerInternal)
	}
	return vpcRecord.CidrBlock, nil
}

// mainRouteTable returns the VPC's main route table, or (nil, nil) if none exists.
// Applies AWS implicit-main semantics; deterministic across duplicate IsMain=true
// records via preferMain.
func (s *RouteTableServiceImpl) mainRouteTable(ctx context.Context, accountID, vpcID string) (*RouteTableRecord, error) {
	rts, err := s.allRouteTablesForVPC(ctx, accountID, vpcID)
	if err != nil {
		return nil, err
	}
	var main *RouteTableRecord
	for i := range rts {
		if rts[i].IsMain && preferMain(main, &rts[i]) {
			main = &rts[i]
		}
	}
	return main, nil
}

// subnetsImplicitlyOnMainRT returns SubnetIds in vpcID that have no explicit
// non-main association on any RT — they inherit the main RT's routes per AWS
// semantics, so main-RT route events must fan out to them.
func (s *RouteTableServiceImpl) subnetsImplicitlyOnMainRT(ctx context.Context, accountID, vpcID string) ([]string, error) {
	rts, err := s.allRouteTablesForVPC(ctx, accountID, vpcID)
	if err != nil {
		return nil, err
	}
	explicit := map[string]bool{}
	for _, rt := range rts {
		for _, assoc := range rt.Associations {
			if assoc.SubnetId == "" || assoc.Main {
				continue
			}
			explicit[assoc.SubnetId] = true
		}
	}

	prefix := accountID + "."
	keys, err := s.subnetKV.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("list subnet keys: %w", err)
	}
	var implicit []string
	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := s.subnetKV.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return nil, fmt.Errorf("read subnet %s: %w", key, err)
		}
		var subnet handlers_ec2_vpc.SubnetRecord
		if err := json.Unmarshal(entry.Value(), &subnet); err != nil {
			slog.WarnContext(ctx, "subnetsImplicitlyOnMainRT: corrupt subnet record", "key", key, "err", err)
			continue
		}
		if subnet.VpcId != vpcID || subnet.SubnetId == "" {
			continue
		}
		if explicit[subnet.SubnetId] {
			continue
		}
		implicit = append(implicit, subnet.SubnetId)
	}
	return implicit, nil
}

// effectiveRouteTable returns the route table whose routes apply to subnetID per AWS
// semantics: explicit non-main association if present, otherwise the VPC's main RT.
// Returns (nil, nil) when neither is present. Main-RT ties broken via preferMain.
func (s *RouteTableServiceImpl) effectiveRouteTable(ctx context.Context, accountID, vpcID, subnetID string) (*RouteTableRecord, error) {
	rts, err := s.allRouteTablesForVPC(ctx, accountID, vpcID)
	if err != nil {
		return nil, err
	}
	var main *RouteTableRecord
	for i := range rts {
		for _, assoc := range rts[i].Associations {
			if assoc.SubnetId == subnetID && !assoc.Main {
				return &rts[i], nil
			}
		}
		if rts[i].IsMain && preferMain(main, &rts[i]) {
			main = &rts[i]
		}
	}
	return main, nil
}

// preferMain reports whether candidate should replace current as the chosen main RT.
// Tiebreakers: (1) more routes, (2) oldest CreatedAt, (3) smaller RouteTableId —
// giving a deterministic total order across duplicate IsMain=true records.
func preferMain(current, candidate *RouteTableRecord) bool {
	if current == nil {
		return true
	}
	if len(candidate.Routes) != len(current.Routes) {
		return len(candidate.Routes) > len(current.Routes)
	}
	if !candidate.CreatedAt.Equal(current.CreatedAt) {
		return candidate.CreatedAt.Before(current.CreatedAt)
	}
	return candidate.RouteTableId < current.RouteTableId
}

// subnetHasDefaultEgress reports whether subnetID's effective RT carries a destCidr
// route pointing at an IGW or NAT GW; false means the subnet must be gated with a DROP policy.
func (s *RouteTableServiceImpl) subnetHasDefaultEgress(ctx context.Context, accountID, vpcID, subnetID, destCidr string) (bool, error) {
	rt, err := s.effectiveRouteTable(ctx, accountID, vpcID, subnetID)
	if err != nil {
		return false, err
	}
	if rt == nil {
		return false, nil
	}
	for _, r := range rt.Routes {
		if r.DestinationCidrBlock != destCidr {
			continue
		}
		if r.GatewayId != "" && strings.HasPrefix(r.GatewayId, "igw-") {
			return true, nil
		}
		if r.NatGatewayId != "" {
			return true, nil
		}
	}
	return false, nil
}

// publishSubnetEgressGateDecision emits vpc.gate-subnet-egress or vpc.ungate-subnet-egress
// for one subnet+destCidr based on whether the effective RT has an internet-bound route.
// Side-effect only; errors are logged.
func (s *RouteTableServiceImpl) publishSubnetEgressGateDecision(ctx context.Context, accountID, vpcID, subnetID, destCidr string) {
	if s.natsConn == nil || subnetID == "" || vpcID == "" {
		return
	}
	if destCidr == "" {
		destCidr = "0.0.0.0/0"
	}
	hasEgress, err := s.subnetHasDefaultEgress(ctx, accountID, vpcID, subnetID, destCidr)
	if err != nil {
		slog.WarnContext(ctx, "subnet egress gate: effective RT lookup failed",
			"vpcId", vpcID, "subnetId", subnetID, "err", err)
		return
	}
	topic := "vpc.gate-subnet-egress"
	if hasEgress {
		topic = "vpc.ungate-subnet-egress"
	}
	evt := struct {
		VpcId           string `json:"vpc_id"`
		SubnetId        string `json:"subnet_id"`
		DestinationCidr string `json:"destination_cidr"`
	}{
		VpcId:           vpcID,
		SubnetId:        subnetID,
		DestinationCidr: destCidr,
	}
	data, err := json.Marshal(evt)
	if err != nil {
		slog.WarnContext(ctx, "subnet egress gate: marshal failed", "topic", topic, "err", err)
		return
	}
	if err := s.natsConn.Publish(topic, data); err != nil {
		slog.WarnContext(ctx, "subnet egress gate: publish failed", "topic", topic, "subnetId", subnetID, "err", err)
		return
	}
	slog.InfoContext(ctx, "subnet egress gate decision published",
		"topic", topic, "vpcId", vpcID, "subnetId", subnetID, "destinationCidr", destCidr)
}

// PublishGateDecisionsForVPC recomputes gate/ungate for every subnet in vpcID against
// destCidr, resolving each subnet's effective RT. Called on IGW Attach/Detach.
// destCidr defaults to 0.0.0.0/0.
func (s *RouteTableServiceImpl) PublishGateDecisionsForVPC(accountID, vpcID, destCidr string) {
	s.publishGateDecisionsForVPC(context.Background(), accountID, vpcID, destCidr)
}

func (s *RouteTableServiceImpl) publishGateDecisionsForVPC(ctx context.Context, accountID, vpcID, destCidr string) {
	if s.natsConn == nil || vpcID == "" {
		return
	}
	if destCidr == "" {
		destCidr = "0.0.0.0/0"
	}
	subnets, err := s.allSubnetsForVPC(ctx, accountID, vpcID)
	if err != nil {
		slog.WarnContext(ctx, "PublishGateDecisionsForVPC: enumerate subnets failed",
			"vpcId", vpcID, "err", err)
		return
	}
	for _, subnetID := range subnets {
		s.publishSubnetEgressGateDecision(ctx, accountID, vpcID, subnetID, destCidr)
	}
}

// allSubnetsForVPC returns every SubnetId in vpcID by scanning the subnet KV.
// Order is unspecified.
func (s *RouteTableServiceImpl) allSubnetsForVPC(ctx context.Context, accountID, vpcID string) ([]string, error) {
	prefix := accountID + "."
	keys, err := s.subnetKV.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("list subnet keys: %w", err)
	}
	var subnets []string
	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := s.subnetKV.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return nil, fmt.Errorf("read subnet %s: %w", key, err)
		}
		var subnet handlers_ec2_vpc.SubnetRecord
		if err := json.Unmarshal(entry.Value(), &subnet); err != nil {
			slog.Warn("allSubnetsForVPC: corrupt subnet record", "key", key, "err", err)
			continue
		}
		if subnet.VpcId != vpcID || subnet.SubnetId == "" {
			continue
		}
		subnets = append(subnets, subnet.SubnetId)
	}
	return subnets, nil
}

// publishSubnetEgressGateDecisionForRT recomputes gate/ungate per subnet in a
// route table (explicit associations + implicit-main subnets if the RT is
// main). destCidr is the route prefix whose presence/absence drove the call.
func (s *RouteTableServiceImpl) publishSubnetEgressGateDecisionForRT(ctx context.Context, accountID string, record *RouteTableRecord, destCidr string) {
	if s.natsConn == nil || record == nil {
		return
	}
	seen := map[string]bool{}
	for _, assoc := range record.Associations {
		if assoc.SubnetId == "" || assoc.Main || seen[assoc.SubnetId] {
			continue
		}
		seen[assoc.SubnetId] = true
		s.publishSubnetEgressGateDecision(ctx, accountID, record.VpcId, assoc.SubnetId, destCidr)
	}
	if !record.IsMain {
		return
	}
	implicit, err := s.subnetsImplicitlyOnMainRT(ctx, accountID, record.VpcId)
	if err != nil {
		slog.WarnContext(ctx, "subnet egress gate: enumerate implicit main-RT subnets failed",
			"vpcId", record.VpcId, "err", err)
		return
	}
	for _, subnetID := range implicit {
		if seen[subnetID] {
			continue
		}
		s.publishSubnetEgressGateDecision(ctx, accountID, record.VpcId, subnetID, destCidr)
	}
}

// allRouteTablesForVPC returns all route tables belonging to a VPC.
func (s *RouteTableServiceImpl) allRouteTablesForVPC(ctx context.Context, accountID, vpcID string) ([]RouteTableRecord, error) {
	prefix := accountID + "."
	keys, err := s.rtbKV.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	var results []RouteTableRecord
	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := s.rtbKV.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue // deleted between Keys() and Get()
			}
			slog.ErrorContext(ctx, "Failed to read route table during VPC scan", "key", key, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		var record RouteTableRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.ErrorContext(ctx, "Corrupt route table record", "key", key, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		if record.VpcId == vpcID {
			results = append(results, record)
		}
	}
	return results, nil
}

// CreateRouteTableForVPC creates a route table with the local route pre-populated.
// Exported for use by VPC service when creating default VPCs.
func (s *RouteTableServiceImpl) CreateRouteTableForVPC(vpcID, vpcCidr, accountID string, isMain bool, rtbID string) (*RouteTableRecord, error) {
	if rtbID == "" {
		rtbID = utils.GenerateResourceID("rtb")
	}

	record := RouteTableRecord{
		RouteTableId: rtbID,
		VpcId:        vpcID,
		AccountID:    accountID,
		IsMain:       isMain,
		Routes: []RouteRecord{
			{
				DestinationCidrBlock: vpcCidr,
				GatewayId:            "local",
				State:                "active",
				Origin:               "CreateRouteTable",
			},
		},
		Tags:      make(map[string]string),
		CreatedAt: time.Now(),
	}

	if isMain {
		record.Associations = []AssociationRecord{
			{
				AssociationId: utils.GenerateResourceID("rtbassoc"),
				Main:          true,
			},
		}
	}

	if err := s.putRouteTable(context.Background(), accountID, &record); err != nil {
		return nil, err
	}

	slog.Info("Created route table", "routeTableId", rtbID, "vpcId", vpcID, "isMain", isMain, "accountID", accountID)
	return &record, nil
}

// CreateRouteTable creates a new custom (non-main) route table for a VPC.
func (s *RouteTableServiceImpl) CreateRouteTable(ctx context.Context, input *ec2.CreateRouteTableInput, accountID string) (*ec2.CreateRouteTableOutput, error) {
	if input.VpcId == nil || *input.VpcId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	vpcID := *input.VpcId
	vpcCidr, err := s.getVPCCidr(ctx, accountID, vpcID)
	if err != nil {
		return nil, err
	}

	record, err := s.CreateRouteTableForVPC(vpcID, vpcCidr, accountID, false, "")
	if err != nil {
		return nil, err
	}

	// Persist tags from the create spec so tag-filtered describes (and the
	// tag-driven CP-VPC teardown that reclaims the NAT-GW EIP) can find this
	// route table.
	if tags := utils.ExtractTags(input.TagSpecifications, ec2.ResourceTypeRouteTable); len(tags) > 0 {
		record.Tags = tags
		if err := s.putRouteTable(ctx, accountID, record); err != nil {
			return nil, err
		}
	}

	return &ec2.CreateRouteTableOutput{
		RouteTable: recordToEC2(record),
	}, nil
}

// DeleteRouteTable deletes a route table (must not be main, must have no subnet associations).
func (s *RouteTableServiceImpl) DeleteRouteTable(ctx context.Context, input *ec2.DeleteRouteTableInput, accountID string) (*ec2.DeleteRouteTableOutput, error) {
	if input.RouteTableId == nil || *input.RouteTableId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	rtbID := *input.RouteTableId
	// AWS-faithful: getRouteTable maps an absent route table to NotFound (the
	// provider tolerates it on destroy); destroy orchestration tolerates it too.
	record, err := s.getRouteTable(ctx, accountID, rtbID)
	if err != nil {
		return nil, err
	}

	if record.IsMain {
		return nil, errors.New(awserrors.ErrorDependencyViolation)
	}

	// Check for non-main associations (subnets still using this table)
	for _, assoc := range record.Associations {
		if !assoc.Main && assoc.SubnetId != "" {
			return nil, errors.New(awserrors.ErrorDependencyViolation)
		}
	}

	if err := s.rtbKV.Delete(ctx, utils.AccountKey(accountID, rtbID)); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "DeleteRouteTable completed", "routeTableId", rtbID, "accountID", accountID)
	return &ec2.DeleteRouteTableOutput{}, nil
}

var describeRouteTablesValidFilters = map[string]bool{
	"vpc-id":                                 true,
	"route-table-id":                         true,
	"association.main":                       true,
	"association.route-table-association-id": true,
	"association.subnet-id":                  true,
	"route.destination-cidr-block":           true,
	"route.gateway-id":                       true,
	"route.nat-gateway-id":                   true,
	"route.state":                            true,
	"route.origin":                           true,
	"owner-id":                               true,
}

// DescribeRouteTables lists route tables, optionally filtered.
func (s *RouteTableServiceImpl) DescribeRouteTables(ctx context.Context, input *ec2.DescribeRouteTablesInput, accountID string) (*ec2.DescribeRouteTablesOutput, error) {
	rtbIDs := make(map[string]bool)
	for _, id := range input.RouteTableIds {
		if id != nil {
			rtbIDs[*id] = true
		}
	}

	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeRouteTablesValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeRouteTables: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	prefix := accountID + "."
	keys, err := s.rtbKV.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	var routeTables []*ec2.RouteTable
	foundIDs := make(map[string]bool)

	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}

		entry, err := s.rtbKV.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			slog.ErrorContext(ctx, "Failed to read route table during describe", "key", key, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		var record RouteTableRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.ErrorContext(ctx, "Corrupt route table record", "key", key, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		if len(rtbIDs) > 0 && !rtbIDs[record.RouteTableId] {
			continue
		}

		if !rtbMatchesFilters(&record, parsedFilters) {
			continue
		}

		routeTables = append(routeTables, recordToEC2(&record))
		foundIDs[record.RouteTableId] = true
	}

	// Return error if specific IDs were requested but not found
	for id := range rtbIDs {
		if !foundIDs[id] {
			return nil, errors.New(awserrors.ErrorInvalidRouteTableIDNotFound)
		}
	}

	return &ec2.DescribeRouteTablesOutput{
		RouteTables: routeTables,
	}, nil
}

// CreateRoute adds a route to a route table.
func (s *RouteTableServiceImpl) CreateRoute(ctx context.Context, input *ec2.CreateRouteInput, accountID string) (*ec2.CreateRouteOutput, error) {
	if input.RouteTableId == nil || *input.RouteTableId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.DestinationCidrBlock == nil || *input.DestinationCidrBlock == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	rtbID := *input.RouteTableId
	destCidr := *input.DestinationCidrBlock

	record, err := s.getRouteTable(ctx, accountID, rtbID)
	if err != nil {
		return nil, err
	}

	// Check for duplicate destination
	for _, r := range record.Routes {
		if r.DestinationCidrBlock == destCidr {
			return nil, errors.New(awserrors.ErrorRouteAlreadyExists)
		}
	}

	var route RouteRecord

	switch {
	case input.GatewayId != nil && *input.GatewayId != "":
		igwID := *input.GatewayId
		// Verify IGW exists and is attached to the same VPC
		igwEntry, err := s.igwKV.Get(ctx, utils.AccountKey(accountID, igwID))
		if err != nil {
			return nil, errors.New(awserrors.ErrorInvalidInternetGatewayIDNotFound)
		}
		var igwRecord handlers_ec2_igw.IGWRecord
		if err := json.Unmarshal(igwEntry.Value(), &igwRecord); err != nil {
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		// AWS rejects a route to an internet gateway that is not attached to the
		// route table's VPC (unattached, or attached to a different VPC) with
		// Gateway.NotAttached — not a bare InvalidParameterValue.
		if igwRecord.VpcId != record.VpcId {
			return nil, errors.New(awserrors.ErrorGatewayNotAttached)
		}
		route = RouteRecord{
			DestinationCidrBlock: destCidr,
			GatewayId:            igwID,
			State:                "active",
			Origin:               "CreateRoute",
		}

		// Publish vpc.add-igw-route events for each subnet associated with this
		// route table so the network subscriber installs per-subnet egress policies.
		s.publishIGWRouteEvents(ctx, accountID, "vpc.add-igw-route", record, igwRecord.VpcId, igwID, destCidr)

	case input.NatGatewayId != nil && *input.NatGatewayId != "":
		natgwID := *input.NatGatewayId
		// Verify NAT GW exists and belongs to the same VPC
		natgwEntry, err := s.natgwKV.Get(ctx, utils.AccountKey(accountID, natgwID))
		if err != nil {
			return nil, errors.New(awserrors.ErrorInvalidNatGatewayIDNotFound)
		}
		var natgwRecord struct {
			NatGatewayId string `json:"nat_gateway_id"`
			VpcId        string `json:"vpc_id"`
			PublicIp     string `json:"public_ip"`
		}
		if err := json.Unmarshal(natgwEntry.Value(), &natgwRecord); err != nil {
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		if natgwRecord.VpcId != record.VpcId {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		route = RouteRecord{
			DestinationCidrBlock: destCidr,
			NatGatewayId:         natgwID,
			State:                "active",
			Origin:               "CreateRoute",
		}

		// Publish vpc.add-nat-gateway events for each subnet associated with this route table
		s.publishNatGatewayEvents(ctx, accountID, record, natgwRecord.VpcId, natgwID, natgwRecord.PublicIp, destCidr)

	default:
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	record.Routes = append(record.Routes, route)

	if err := s.putRouteTable(ctx, accountID, record); err != nil {
		return nil, err
	}

	// Re-evaluate per-subnet gate decision now that the new route is in KV.
	// publishSubnetEgressGateDecisionForRT skips when natsConn is nil.
	if destCidr == "0.0.0.0/0" {
		s.publishSubnetEgressGateDecisionForRT(ctx, accountID, record, destCidr)
	}

	slog.InfoContext(ctx, "CreateRoute completed", "routeTableId", rtbID, "destination", destCidr, "accountID", accountID)

	return &ec2.CreateRouteOutput{
		Return: aws.Bool(true),
	}, nil
}

// DeleteRoute removes a route from a route table (cannot delete local route).
func (s *RouteTableServiceImpl) DeleteRoute(ctx context.Context, input *ec2.DeleteRouteInput, accountID string) (*ec2.DeleteRouteOutput, error) {
	if input.RouteTableId == nil || *input.RouteTableId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.DestinationCidrBlock == nil || *input.DestinationCidrBlock == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	rtbID := *input.RouteTableId
	destCidr := *input.DestinationCidrBlock

	record, err := s.getRouteTable(ctx, accountID, rtbID)
	if err != nil {
		return nil, err
	}

	idx := -1
	for i, r := range record.Routes {
		if r.DestinationCidrBlock == destCidr {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, errors.New(awserrors.ErrorInvalidRouteNotFound)
	}

	departing := record.Routes[idx]

	// Cannot delete local route
	if departing.GatewayId == "local" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	record.Routes = append(record.Routes[:idx], record.Routes[idx+1:]...)

	if err := s.putRouteTable(ctx, accountID, record); err != nil {
		return nil, err
	}

	if departing.GatewayId != "" && strings.HasPrefix(departing.GatewayId, "igw-") {
		s.publishIGWRouteEvents(ctx, accountID, "vpc.delete-igw-route", record, record.VpcId, departing.GatewayId, departing.DestinationCidrBlock)
	}

	if departing.NatGatewayId != "" {
		s.publishNatGatewayDeleteEvents(ctx, accountID, record, departing.NatGatewayId, departing.DestinationCidrBlock)
	}

	// Re-evaluate gate decision: if the deleted route was 0.0.0.0/0 and no
	// other egress target remains in the effective RT, subnets that were
	// allowed now need a drop policy installed.
	if departing.DestinationCidrBlock == "0.0.0.0/0" {
		s.publishSubnetEgressGateDecisionForRT(ctx, accountID, record, departing.DestinationCidrBlock)
	}

	slog.InfoContext(ctx, "DeleteRoute completed", "routeTableId", rtbID, "destination", destCidr, "accountID", accountID)

	return &ec2.DeleteRouteOutput{}, nil
}

// ReplaceRoute atomically replaces the target of an existing route.
func (s *RouteTableServiceImpl) ReplaceRoute(ctx context.Context, input *ec2.ReplaceRouteInput, accountID string) (*ec2.ReplaceRouteOutput, error) {
	if input.RouteTableId == nil || *input.RouteTableId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.DestinationCidrBlock == nil || *input.DestinationCidrBlock == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	rtbID := *input.RouteTableId
	destCidr := *input.DestinationCidrBlock

	record, err := s.getRouteTable(ctx, accountID, rtbID)
	if err != nil {
		return nil, err
	}

	idx := -1
	for i, r := range record.Routes {
		if r.DestinationCidrBlock == destCidr {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, errors.New(awserrors.ErrorInvalidRouteNotFound)
	}

	if record.Routes[idx].GatewayId == "local" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// V1: only GatewayId target supported
	if input.GatewayId == nil || *input.GatewayId == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	igwID := *input.GatewayId

	// Verify IGW exists and is attached to same VPC
	igwEntry, err := s.igwKV.Get(ctx, utils.AccountKey(accountID, igwID))
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidInternetGatewayIDNotFound)
	}
	var igwRecord handlers_ec2_igw.IGWRecord
	if err := json.Unmarshal(igwEntry.Value(), &igwRecord); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if igwRecord.VpcId != record.VpcId {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	record.Routes[idx].GatewayId = igwID
	record.Routes[idx].NatGatewayId = ""
	record.Routes[idx].State = "active"

	if err := s.putRouteTable(ctx, accountID, record); err != nil {
		return nil, err
	}

	slog.InfoContext(ctx, "ReplaceRoute completed", "routeTableId", rtbID, "destination", destCidr, "gatewayId", igwID, "accountID", accountID)

	return &ec2.ReplaceRouteOutput{}, nil
}

// AssociateRouteTable associates a subnet with a route table.
func (s *RouteTableServiceImpl) AssociateRouteTable(ctx context.Context, input *ec2.AssociateRouteTableInput, accountID string) (*ec2.AssociateRouteTableOutput, error) {
	if input.RouteTableId == nil || *input.RouteTableId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.SubnetId == nil || *input.SubnetId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	rtbID := *input.RouteTableId
	subnetID := *input.SubnetId

	record, err := s.getRouteTable(ctx, accountID, rtbID)
	if err != nil {
		return nil, err
	}

	// Verify subnet exists and belongs to the same VPC
	subnetEntry, err := s.subnetKV.Get(ctx, utils.AccountKey(accountID, subnetID))
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidSubnetIDNotFound)
	}
	var subnetRecord handlers_ec2_vpc.SubnetRecord
	if err := json.Unmarshal(subnetEntry.Value(), &subnetRecord); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if subnetRecord.VpcId != record.VpcId {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// Check the subnet doesn't already have an explicit association in any route table
	allTables, err := s.allRouteTablesForVPC(ctx, accountID, record.VpcId)
	if err != nil {
		return nil, err
	}
	for _, table := range allTables {
		for _, assoc := range table.Associations {
			if assoc.SubnetId == subnetID && !assoc.Main {
				return nil, errors.New(awserrors.ErrorResourceAlreadyAssociated)
			}
		}
	}

	assocID := utils.GenerateResourceID("rtbassoc")
	if err := s.mutateRouteTableCAS(ctx, accountID, rtbID, func(rec *RouteTableRecord) (bool, error) {
		// Re-check same-table association under the fresh read: a concurrent
		// associate for this subnet may have landed since the precheck.
		for _, assoc := range rec.Associations {
			if assoc.SubnetId == subnetID && !assoc.Main {
				return false, errors.New(awserrors.ErrorResourceAlreadyAssociated)
			}
		}
		rec.Associations = append(rec.Associations, AssociationRecord{
			AssociationId: assocID,
			SubnetId:      subnetID,
			Main:          false,
		})
		return true, nil
	}); err != nil {
		return nil, err
	}

	// Subnet leaves implicit main-RT membership: tear down main RT's per-subnet rules
	// before the explicit RT's rules take effect.
	if !record.IsMain {
		if mainRT, err := s.mainRouteTable(ctx, accountID, record.VpcId); err != nil {
			slog.WarnContext(ctx, "AssociateRouteTable: main RT lookup failed", "vpcId", record.VpcId, "err", err)
		} else if mainRT != nil {
			s.publishNatGatewayEventsForAssociation(ctx, accountID, "vpc.delete-nat-gateway", mainRT, subnetID)
			s.publishIGWRouteEventsForAssociation(ctx, accountID, "vpc.delete-igw-route", mainRT, subnetID)
		}
	}

	// Terraform commonly creates the route table + NAT GW route before associating
	// subnets. CreateRoute runs against a table with zero associations so no SNAT
	// events fire, so we must emit them here once the subnet joins.
	s.publishNatGatewayEventsForAssociation(ctx, accountID, "vpc.add-nat-gateway", record, subnetID)
	s.publishIGWRouteEventsForAssociation(ctx, accountID, "vpc.add-igw-route", record, subnetID)

	// Subnet's effective RT just changed — recompute gate decision.
	s.publishSubnetEgressGateDecision(ctx, accountID, record.VpcId, subnetID, "0.0.0.0/0")

	slog.InfoContext(ctx, "AssociateRouteTable completed", "routeTableId", rtbID, "subnetId", subnetID, "associationId", assocID, "accountID", accountID)

	return &ec2.AssociateRouteTableOutput{
		AssociationId: aws.String(assocID),
		AssociationState: &ec2.RouteTableAssociationState{
			State: aws.String("associated"),
		},
	}, nil
}

// DisassociateRouteTable removes a subnet association (cannot disassociate main).
func (s *RouteTableServiceImpl) DisassociateRouteTable(ctx context.Context, input *ec2.DisassociateRouteTableInput, accountID string) (*ec2.DisassociateRouteTableOutput, error) {
	if input.AssociationId == nil || *input.AssociationId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	assocID := *input.AssociationId

	// Search all route tables for this account to find the association
	prefix := accountID + "."
	keys, err := s.rtbKV.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, errors.New(awserrors.ErrorInvalidAssociationIDNotFound)
		}
		slog.ErrorContext(ctx, "Failed to list route table keys", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}

		entry, err := s.rtbKV.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			slog.ErrorContext(ctx, "Failed to read route table during disassociate scan", "key", key, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		var record RouteTableRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.ErrorContext(ctx, "Corrupt route table record", "key", key, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		for _, assoc := range record.Associations {
			if assoc.AssociationId == assocID {
				if assoc.Main {
					return nil, errors.New(awserrors.ErrorInvalidParameterValue)
				}
				departingSubnetID := assoc.SubnetId
				if err := s.mutateRouteTableCAS(ctx, accountID, record.RouteTableId, func(rec *RouteTableRecord) (bool, error) {
					for j, a := range rec.Associations {
						if a.AssociationId == assocID {
							rec.Associations = append(rec.Associations[:j], rec.Associations[j+1:]...)
							return true, nil
						}
					}
					return false, nil // already removed by a concurrent writer — idempotent
				}); err != nil {
					return nil, err
				}

				// Tear down per-subnet SNAT rules for any NAT GW routes on this table.
				s.publishNatGatewayEventsForAssociation(ctx, accountID, "vpc.delete-nat-gateway", &record, departingSubnetID)
				s.publishIGWRouteEventsForAssociation(ctx, accountID, "vpc.delete-igw-route", &record, departingSubnetID)

				// Subnet falls back to implicit main-RT membership: re-install main RT's per-subnet rules.
				if mainRT, err := s.mainRouteTable(ctx, accountID, record.VpcId); err != nil {
					slog.WarnContext(ctx, "DisassociateRouteTable: main RT lookup failed", "vpcId", record.VpcId, "err", err)
				} else if mainRT != nil && mainRT.RouteTableId != record.RouteTableId {
					s.publishNatGatewayEventsForAssociation(ctx, accountID, "vpc.add-nat-gateway", mainRT, departingSubnetID)
					s.publishIGWRouteEventsForAssociation(ctx, accountID, "vpc.add-igw-route", mainRT, departingSubnetID)
				}

				// Subnet's effective RT just changed — recompute gate decision.
				s.publishSubnetEgressGateDecision(ctx, accountID, record.VpcId, departingSubnetID, "0.0.0.0/0")

				slog.InfoContext(ctx, "DisassociateRouteTable completed", "associationId", assocID, "routeTableId", record.RouteTableId, "accountID", accountID)
				return &ec2.DisassociateRouteTableOutput{}, nil
			}
		}
	}

	return nil, errors.New(awserrors.ErrorInvalidAssociationIDNotFound)
}

// ReplaceRouteTableAssociation atomically moves a subnet from one route table to another.
func (s *RouteTableServiceImpl) ReplaceRouteTableAssociation(ctx context.Context, input *ec2.ReplaceRouteTableAssociationInput, accountID string) (*ec2.ReplaceRouteTableAssociationOutput, error) {
	if input.AssociationId == nil || *input.AssociationId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.RouteTableId == nil || *input.RouteTableId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	assocID := *input.AssociationId
	newRtbID := *input.RouteTableId

	// Verify the new route table exists
	newRecord, err := s.getRouteTable(ctx, accountID, newRtbID)
	if err != nil {
		return nil, err
	}

	// Find and remove the association from its current route table
	prefix := accountID + "."
	keys, err := s.rtbKV.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, errors.New(awserrors.ErrorInvalidAssociationIDNotFound)
		}
		slog.ErrorContext(ctx, "Failed to list route table keys", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}

		entry, err := s.rtbKV.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			slog.ErrorContext(ctx, "Failed to read route table during replace scan", "key", key, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		var oldRecord RouteTableRecord
		if err := json.Unmarshal(entry.Value(), &oldRecord); err != nil {
			slog.ErrorContext(ctx, "Corrupt route table record", "key", key, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		for i, assoc := range oldRecord.Associations {
			if assoc.AssociationId != assocID {
				continue
			}

			// If replacing a main association, swap main table for the VPC
			if assoc.Main {
				// New table becomes main, old table loses main status
				newRecord.IsMain = true
				oldRecord.IsMain = false
			}

			// Remove from old table
			oldRecord.Associations = append(oldRecord.Associations[:i], oldRecord.Associations[i+1:]...)
			if err := s.putRouteTable(ctx, accountID, &oldRecord); err != nil {
				return nil, err
			}

			// Tear down SNAT for any NAT GW routes on the old table — the
			// subnet is leaving so its per-CIDR rules must be removed before
			// the new table's rules take effect.
			s.publishNatGatewayEventsForAssociation(ctx, accountID, "vpc.delete-nat-gateway", &oldRecord, assoc.SubnetId)
			s.publishIGWRouteEventsForAssociation(ctx, accountID, "vpc.delete-igw-route", &oldRecord, assoc.SubnetId)

			// Add to new table with new ID
			newAssocID := utils.GenerateResourceID("rtbassoc")
			newRecord.Associations = append(newRecord.Associations, AssociationRecord{
				AssociationId: newAssocID,
				SubnetId:      assoc.SubnetId,
				Main:          assoc.Main,
			})
			if err := s.putRouteTable(ctx, accountID, newRecord); err != nil {
				// Restore the old association even when the request context caused the
				// failed write, otherwise cancellation turns a rollback into a no-op.
				oldRecord.Associations = append(oldRecord.Associations, assoc)
				restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				restoreErr := s.putRouteTable(restoreCtx, accountID, &oldRecord)
				cancel()
				if restoreErr != nil {
					slog.ErrorContext(ctx, "CRITICAL: ReplaceRouteTableAssociation partial failure, association lost",
						"associationId", assocID, "oldRouteTableId", oldRecord.RouteTableId,
						"newRouteTableId", newRtbID, "restoreErr", restoreErr, "originalErr", err)
				}
				return nil, err
			}

			// Install SNAT for any NAT GW routes on the new table.
			s.publishNatGatewayEventsForAssociation(ctx, accountID, "vpc.add-nat-gateway", newRecord, assoc.SubnetId)
			s.publishIGWRouteEventsForAssociation(ctx, accountID, "vpc.add-igw-route", newRecord, assoc.SubnetId)

			// Subnet's effective RT just changed — recompute gate decision.
			s.publishSubnetEgressGateDecision(ctx, accountID, newRecord.VpcId, assoc.SubnetId, "0.0.0.0/0")

			slog.InfoContext(ctx, "ReplaceRouteTableAssociation completed",
				"oldAssociationId", assocID, "newAssociationId", newAssocID,
				"newRouteTableId", newRtbID, "accountID", accountID)

			return &ec2.ReplaceRouteTableAssociationOutput{
				NewAssociationId: aws.String(newAssocID),
				AssociationState: &ec2.RouteTableAssociationState{
					State: aws.String("associated"),
				},
			}, nil
		}
	}

	return nil, errors.New(awserrors.ErrorInvalidAssociationIDNotFound)
}

// rtbMatchesFilters checks if a route table record matches all parsed filters.
func rtbMatchesFilters(record *RouteTableRecord, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}
		switch name {
		case "vpc-id":
			if !filterutil.MatchesAny(values, record.VpcId) {
				return false
			}
		case "route-table-id":
			if !filterutil.MatchesAny(values, record.RouteTableId) {
				return false
			}
		case "association.main":
			hasMain := false
			for _, a := range record.Associations {
				if a.Main {
					hasMain = true
					break
				}
			}
			wantMain := filterutil.MatchesAny(values, "true")
			if wantMain != hasMain {
				return false
			}
		case "association.route-table-association-id":
			found := false
			for _, a := range record.Associations {
				if filterutil.MatchesAny(values, a.AssociationId) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		case "association.subnet-id":
			found := false
			for _, a := range record.Associations {
				if filterutil.MatchesAny(values, a.SubnetId) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		case "route.destination-cidr-block":
			found := false
			for _, r := range record.Routes {
				if filterutil.MatchesAny(values, r.DestinationCidrBlock) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		case "route.gateway-id":
			found := false
			for _, r := range record.Routes {
				if filterutil.MatchesAny(values, r.GatewayId) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		case "route.nat-gateway-id":
			found := false
			for _, r := range record.Routes {
				if filterutil.MatchesAny(values, r.NatGatewayId) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		case "route.state":
			found := false
			for _, r := range record.Routes {
				if filterutil.MatchesAny(values, r.State) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		case "route.origin":
			found := false
			for _, r := range record.Routes {
				if filterutil.MatchesAny(values, r.Origin) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		case "owner-id":
			if !filterutil.MatchesAny(values, record.AccountID) {
				return false
			}
		default:
			return false
		}
	}
	return filterutil.MatchesTags(filters, record.Tags)
}

// publishNatGatewayEvents publishes vpc.add-nat-gateway for each associated subnet
// so vpcd creates the SNAT rules.
func (s *RouteTableServiceImpl) publishNatGatewayEvents(ctx context.Context, accountID string, record *RouteTableRecord, vpcID, natgwID, publicIp, destCidr string) {
	if s.natsConn == nil {
		return
	}
	seen := map[string]bool{}
	for _, assoc := range record.Associations {
		if assoc.SubnetId == "" || assoc.Main || seen[assoc.SubnetId] {
			continue
		}
		seen[assoc.SubnetId] = true
		s.publishNatGatewayEventForSubnet(ctx, accountID, "vpc.add-nat-gateway", assoc.SubnetId, vpcID, natgwID, publicIp, destCidr)
	}
	if !record.IsMain {
		return
	}
	implicit, err := s.subnetsImplicitlyOnMainRT(ctx, accountID, vpcID)
	if err != nil {
		slog.WarnContext(ctx, "NAT GW event: enumerate implicit main-RT subnets failed", "topic", "vpc.add-nat-gateway", "vpcId", vpcID, "err", err)
		return
	}
	for _, subnetID := range implicit {
		if seen[subnetID] {
			continue
		}
		s.publishNatGatewayEventForSubnet(ctx, accountID, "vpc.add-nat-gateway", subnetID, vpcID, natgwID, publicIp, destCidr)
	}
}

// publishNatGatewayDeleteEvents fans vpc.delete-nat-gateway out to every
// associated subnet so each per-subnet SNAT rule + egress policy is removed
// when CreateRoute's NATGW route is deleted (mirror of publishNatGatewayEvents).
func (s *RouteTableServiceImpl) publishNatGatewayDeleteEvents(ctx context.Context, accountID string, record *RouteTableRecord, natgwID, destCidr string) {
	if s.natsConn == nil {
		return
	}
	natgwEntry, err := s.natgwKV.Get(ctx, utils.AccountKey(accountID, natgwID))
	if err != nil {
		slog.WarnContext(ctx, "NAT GW event: natgw lookup failed", "topic", "vpc.delete-nat-gateway", "natGatewayId", natgwID, "err", err)
		return
	}
	var natgw struct {
		NatGatewayId string `json:"nat_gateway_id"`
		VpcId        string `json:"vpc_id"`
		PublicIp     string `json:"public_ip"`
	}
	if err := json.Unmarshal(natgwEntry.Value(), &natgw); err != nil {
		slog.WarnContext(ctx, "NAT GW event: natgw unmarshal failed", "topic", "vpc.delete-nat-gateway", "natGatewayId", natgwID, "err", err)
		return
	}
	seen := map[string]bool{}
	for _, assoc := range record.Associations {
		if assoc.SubnetId == "" || assoc.Main || seen[assoc.SubnetId] {
			continue
		}
		seen[assoc.SubnetId] = true
		s.publishNatGatewayEventForSubnet(ctx, accountID, "vpc.delete-nat-gateway", assoc.SubnetId, natgw.VpcId, natgw.NatGatewayId, natgw.PublicIp, destCidr)
	}
	if !record.IsMain {
		return
	}
	implicit, err := s.subnetsImplicitlyOnMainRT(ctx, accountID, natgw.VpcId)
	if err != nil {
		slog.WarnContext(ctx, "NAT GW event: enumerate implicit main-RT subnets failed", "topic", "vpc.delete-nat-gateway", "vpcId", natgw.VpcId, "err", err)
		return
	}
	for _, subnetID := range implicit {
		if seen[subnetID] {
			continue
		}
		s.publishNatGatewayEventForSubnet(ctx, accountID, "vpc.delete-nat-gateway", subnetID, natgw.VpcId, natgw.NatGatewayId, natgw.PublicIp, destCidr)
	}
}

// publishNatGatewayEventsForAssociation emits one NAT GW SNAT event per NAT GW route
// on the table, scoped to a single subnet joining or leaving the table.
func (s *RouteTableServiceImpl) publishNatGatewayEventsForAssociation(ctx context.Context, accountID, topic string, record *RouteTableRecord, subnetID string) {
	if s.natsConn == nil || subnetID == "" {
		return
	}
	for _, r := range record.Routes {
		if r.NatGatewayId == "" {
			continue
		}
		natgwEntry, err := s.natgwKV.Get(ctx, utils.AccountKey(accountID, r.NatGatewayId))
		if err != nil {
			slog.WarnContext(ctx, "NAT GW event: natgw lookup failed", "topic", topic, "natGatewayId", r.NatGatewayId, "err", err)
			continue
		}
		var natgw struct {
			NatGatewayId string `json:"nat_gateway_id"`
			VpcId        string `json:"vpc_id"`
			PublicIp     string `json:"public_ip"`
		}
		if err := json.Unmarshal(natgwEntry.Value(), &natgw); err != nil {
			slog.WarnContext(ctx, "NAT GW event: natgw unmarshal failed", "topic", topic, "natGatewayId", r.NatGatewayId, "err", err)
			continue
		}
		s.publishNatGatewayEventForSubnet(ctx, accountID, topic, subnetID, natgw.VpcId, natgw.NatGatewayId, natgw.PublicIp, r.DestinationCidrBlock)
	}
}

// publishNatGatewayEventForSubnet publishes a single vpc.{add,delete}-nat-gateway
// event for the given subnet. Side-effect only — logs and swallows errors so a
// missing subnet record doesn't fail the caller's API response.
func (s *RouteTableServiceImpl) publishNatGatewayEventForSubnet(ctx context.Context, accountID, topic, subnetID, vpcID, natgwID, publicIp, destCidr string) {
	if s.natsConn == nil {
		return
	}
	subnetEntry, err := s.subnetKV.Get(ctx, utils.AccountKey(accountID, subnetID))
	if err != nil {
		slog.WarnContext(ctx, "NAT GW event: subnet lookup failed", "topic", topic, "subnetId", subnetID, "err", err)
		return
	}
	var subnet handlers_ec2_vpc.SubnetRecord
	if err := json.Unmarshal(subnetEntry.Value(), &subnet); err != nil {
		slog.WarnContext(ctx, "NAT GW event: subnet unmarshal failed", "topic", topic, "subnetId", subnetID, "err", err)
		return
	}
	evt := struct {
		VpcId           string `json:"vpc_id"`
		NatGatewayId    string `json:"nat_gateway_id"`
		PublicIp        string `json:"public_ip"`
		SubnetCidr      string `json:"subnet_cidr"`
		SubnetId        string `json:"subnet_id"`
		DestinationCidr string `json:"destination_cidr"`
	}{
		VpcId:           vpcID,
		NatGatewayId:    natgwID,
		PublicIp:        publicIp,
		SubnetCidr:      subnet.CidrBlock,
		SubnetId:        subnetID,
		DestinationCidr: destCidr,
	}
	data, err := json.Marshal(evt)
	if err != nil {
		slog.WarnContext(ctx, "NAT GW event: marshal failed", "topic", topic, "err", err)
		return
	}
	if err := s.natsConn.Publish(topic, data); err != nil {
		slog.WarnContext(ctx, "NAT GW event: publish failed", "topic", topic, "subnetId", subnetID, "err", err)
		return
	}
	slog.InfoContext(ctx, "NAT GW event published", "topic", topic, "subnetCidr", subnet.CidrBlock, "publicIp", publicIp, "subnetId", subnetID, "destinationCidr", destCidr)
}

// publishIGWRouteEvents emits one vpc.{add,delete}-igw-route event per associated
// subnet when an IGW route is added or removed from a table.
func (s *RouteTableServiceImpl) publishIGWRouteEvents(ctx context.Context, accountID, topic string, record *RouteTableRecord, vpcID, igwID, destCidr string) {
	if s.natsConn == nil {
		return
	}
	seen := map[string]bool{}
	for _, assoc := range record.Associations {
		if assoc.SubnetId == "" || assoc.Main || seen[assoc.SubnetId] {
			continue
		}
		seen[assoc.SubnetId] = true
		s.publishIGWRouteEventForSubnet(ctx, topic, assoc.SubnetId, vpcID, igwID, destCidr)
	}
	if !record.IsMain {
		return
	}
	implicit, err := s.subnetsImplicitlyOnMainRT(ctx, accountID, vpcID)
	if err != nil {
		slog.WarnContext(ctx, "IGW route event: enumerate implicit main-RT subnets failed", "topic", topic, "vpcId", vpcID, "err", err)
		return
	}
	for _, subnetID := range implicit {
		if seen[subnetID] {
			continue
		}
		s.publishIGWRouteEventForSubnet(ctx, topic, subnetID, vpcID, igwID, destCidr)
	}
}

// publishIGWRouteEventsForAssociation emits one IGW route event per IGW route on the
// table, scoped to a single subnet joining or leaving the table.
func (s *RouteTableServiceImpl) publishIGWRouteEventsForAssociation(ctx context.Context, accountID, topic string, record *RouteTableRecord, subnetID string) {
	if s.natsConn == nil || subnetID == "" {
		return
	}
	for _, r := range record.Routes {
		if r.GatewayId == "" || !strings.HasPrefix(r.GatewayId, "igw-") {
			continue
		}
		s.publishIGWRouteEventForSubnet(ctx, topic, subnetID, record.VpcId, r.GatewayId, r.DestinationCidrBlock)
	}
}

// publishIGWRouteEventForSubnet publishes a single vpc.{add,delete}-igw-route event.
// Side-effect only — logs and swallows errors so a publish failure doesn't fail the caller.
func (s *RouteTableServiceImpl) publishIGWRouteEventForSubnet(ctx context.Context, topic, subnetID, vpcID, igwID, destCidr string) {
	if s.natsConn == nil {
		return
	}
	evt := struct {
		VpcId             string `json:"vpc_id"`
		SubnetId          string `json:"subnet_id"`
		DestinationCidr   string `json:"destination_cidr"`
		InternetGatewayId string `json:"internet_gateway_id"`
	}{
		VpcId:             vpcID,
		SubnetId:          subnetID,
		DestinationCidr:   destCidr,
		InternetGatewayId: igwID,
	}
	data, err := json.Marshal(evt)
	if err != nil {
		slog.WarnContext(ctx, "IGW route event: marshal failed", "topic", topic, "err", err)
		return
	}
	if err := s.natsConn.Publish(topic, data); err != nil {
		slog.WarnContext(ctx, "IGW route event: publish failed", "topic", topic, "subnetId", subnetID, "err", err)
		return
	}
	slog.InfoContext(ctx, "IGW route event published", "topic", topic, "subnetId", subnetID, "destinationCidr", destCidr, "igwId", igwID)
}

// recordToEC2 converts an internal record to an AWS SDK RouteTable struct.
func recordToEC2(record *RouteTableRecord) *ec2.RouteTable {
	rtb := &ec2.RouteTable{
		RouteTableId: aws.String(record.RouteTableId),
		VpcId:        aws.String(record.VpcId),
		OwnerId:      aws.String(record.AccountID),
	}

	for _, r := range record.Routes {
		route := &ec2.Route{
			DestinationCidrBlock: aws.String(r.DestinationCidrBlock),
			State:                aws.String(r.State),
			Origin:               aws.String(r.Origin),
		}
		if r.GatewayId != "" {
			route.GatewayId = aws.String(r.GatewayId)
		}
		if r.NatGatewayId != "" {
			route.NatGatewayId = aws.String(r.NatGatewayId)
		}
		rtb.Routes = append(rtb.Routes, route)
	}

	for _, a := range record.Associations {
		assoc := &ec2.RouteTableAssociation{
			RouteTableAssociationId: aws.String(a.AssociationId),
			RouteTableId:            aws.String(record.RouteTableId),
			Main:                    aws.Bool(a.Main),
			AssociationState: &ec2.RouteTableAssociationState{
				State: aws.String("associated"),
			},
		}
		if a.SubnetId != "" {
			assoc.SubnetId = aws.String(a.SubnetId)
		}
		rtb.Associations = append(rtb.Associations, assoc)
	}

	rtb.Tags = utils.MapToEC2Tags(record.Tags)

	return rtb
}

// ApplyRecordTags mirrors CreateTags into the owning route-table KV record so
// tag-filtered describes observe tags added after create. Resource ids this
// service does not own are skipped; absent records are a no-op.
func (s *RouteTableServiceImpl) ApplyRecordTags(input *ec2.CreateTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	return utils.MirrorKVRecordTags(context.Background(), s.rtbKV, accountID, "rtb-", input.Resources,
		func(r *RouteTableRecord) *map[string]string { return &r.Tags },
		utils.MergeTagsMut(input))
}

// RemoveRecordTags mirrors DeleteTags into the owning route-table KV record
// with AWS-faithful delete semantics.
func (s *RouteTableServiceImpl) RemoveRecordTags(input *ec2.DeleteTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	return utils.MirrorKVRecordTags(context.Background(), s.rtbKV, accountID, "rtb-", input.Resources,
		func(r *RouteTableRecord) *map[string]string { return &r.Tags },
		utils.RemoveTagsMut(input))
}
