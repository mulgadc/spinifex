package handlers_ec2_vpc

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// KVBucketDefaultVPCs holds one claim per account, keyed by account ID,
	// naming the IDs of its default resources.
	KVBucketDefaultVPCs        = "spinifex-vpc-defaults"
	KVBucketDefaultVPCsVersion = 1

	// defaultClaimAttempts bounds how often a claim can be lost or retired
	// underneath one EnsureDefaultVPC call.
	defaultClaimAttempts = 4
)

// DefaultVPCInfo holds the IDs of an account's default resources.
type DefaultVPCInfo struct {
	VpcId             string
	SubnetId          string
	Cidr              string
	SubnetCidr        string
	InternetGatewayId string
}

// BootstrapIDs holds pre-generated resource IDs from the [bootstrap] config.
// When provided, EnsureDefaultVPC uses these IDs instead of generating random ones,
// ensuring consistency between admin init, daemon, and vpcd.
type BootstrapIDs struct {
	VpcId    string
	SubnetId string
	IgwId    string
}

// defaultVPCClaim is the single agreed set of default resource IDs for an
// account. It is written with Create, so nodes racing to build the defaults all
// build the winner's IDs instead of one set each.
type defaultVPCClaim struct {
	VpcId             string    `json:"vpc_id"`
	SubnetId          string    `json:"subnet_id,omitempty"`
	GroupId           string    `json:"group_id,omitempty"`
	RouteTableId      string    `json:"route_table_id,omitempty"`
	InternetGatewayId string    `json:"internet_gateway_id"`
	Cidr              string    `json:"cidr"`
	SubnetCidr        string    `json:"subnet_cidr,omitempty"`
	VNI               int64     `json:"vni,omitempty"`
	Complete          bool      `json:"complete"`
	CreatedAt         time.Time `json:"created_at"`
}

func (c *defaultVPCClaim) info() *DefaultVPCInfo {
	return &DefaultVPCInfo{
		VpcId:             c.VpcId,
		SubnetId:          c.SubnetId,
		Cidr:              c.Cidr,
		SubnetCidr:        c.SubnetCidr,
		InternetGatewayId: c.InternetGatewayId,
	}
}

// EnsureDefaultVPC creates a default VPC and subnet if none exists for the
// account. Safe to call multiple times, from any number of nodes at once.
func (s *VPCServiceImpl) EnsureDefaultVPC(accountID string, bootstrap ...BootstrapIDs) (*DefaultVPCInfo, error) {
	return s.ensureDefaultVPC(context.Background(), accountID, bootstrap...)
}

func (s *VPCServiceImpl) ensureDefaultVPC(ctx context.Context, accountID string, bootstrap ...BootstrapIDs) (*DefaultVPCInfo, error) {
	if s.vpcKV == nil || s.defaultsKV == nil {
		return nil, nil // No persistence, skip
	}
	var ids BootstrapIDs
	if len(bootstrap) > 0 {
		ids = bootstrap[0]
	}

	for range defaultClaimAttempts {
		claim, revision, err := s.readDefaultVPCClaim(ctx, accountID)
		if err != nil {
			return nil, err
		}
		if claim == nil {
			if err := s.proposeDefaultVPC(ctx, accountID, ids); err != nil {
				return nil, err
			}
			continue // read back whichever proposal won
		}

		if !claim.Complete {
			if err := s.buildDefaultVPC(ctx, accountID, claim); err != nil {
				return nil, err
			}
			return claim.info(), nil
		}

		if _, err := s.vpcKV.Get(ctx, utils.AccountKey(accountID, claim.VpcId)); err == nil {
			return claim.info(), nil
		} else if !errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, fmt.Errorf("read default VPC %s: %w", claim.VpcId, err)
		}
		// The default VPC was deleted without its claim. Retire the claim so
		// the next pass builds a new one rather than reporting a missing VPC.
		if err := s.defaultsKV.Delete(ctx, accountID, jetstream.LastRevision(revision)); err != nil &&
			!errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			return nil, fmt.Errorf("retire stale default VPC claim: %w", err)
		}
	}
	return nil, fmt.Errorf("default VPC claim for account %s did not settle after %d attempts", accountID, defaultClaimAttempts)
}

// DefaultVPC returns the account's completed default resources, or nil when
// EnsureDefaultVPC has not finished building them.
func (s *VPCServiceImpl) DefaultVPC(ctx context.Context, accountID string) (*DefaultVPCInfo, error) {
	if s.defaultsKV == nil {
		return nil, nil
	}
	claim, _, err := s.readDefaultVPCClaim(ctx, accountID)
	if err != nil || claim == nil || !claim.Complete {
		return nil, err
	}
	return claim.info(), nil
}

func (s *VPCServiceImpl) readDefaultVPCClaim(ctx context.Context, accountID string) (*defaultVPCClaim, uint64, error) {
	entry, err := s.defaultsKV.Get(ctx, accountID)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read default VPC claim: %w", err)
	}
	var claim defaultVPCClaim
	if err := json.Unmarshal(entry.Value(), &claim); err != nil {
		return nil, 0, fmt.Errorf("decode default VPC claim: %w", err)
	}
	return &claim, entry.Revision(), nil
}

// proposeDefaultVPC offers a claim for an account that has none. Losing the
// Create to another node is not an error, and the VNI a losing proposal
// allocated is simply never used.
func (s *VPCServiceImpl) proposeDefaultVPC(ctx context.Context, accountID string, ids BootstrapIDs) error {
	claim, err := s.adoptExistingDefaultVPC(ctx, accountID, ids)
	if err != nil {
		return err
	}
	if claim == nil {
		vni, err := s.nextVNI(ctx)
		if err != nil {
			return fmt.Errorf("allocate VNI for default VPC: %w", err)
		}
		claim = &defaultVPCClaim{
			VpcId:             cmp.Or(ids.VpcId, utils.GenerateResourceID("vpc")),
			SubnetId:          cmp.Or(ids.SubnetId, utils.GenerateResourceID("subnet")),
			GroupId:           utils.GenerateResourceID("sg"),
			RouteTableId:      utils.GenerateResourceID("rtb"),
			InternetGatewayId: cmp.Or(ids.IgwId, utils.GenerateResourceID("igw")),
			Cidr:              DefaultVPCCidr,
			SubnetCidr:        DefaultSubnetCidr,
			VNI:               vni,
			CreatedAt:         time.Now(),
		}
	}

	data, err := json.Marshal(claim)
	if err != nil {
		return fmt.Errorf("marshal default VPC claim: %w", err)
	}
	if _, err := s.defaultsKV.Create(ctx, accountID, data); err != nil && !errors.Is(err, jetstream.ErrKeyExists) {
		return fmt.Errorf("store default VPC claim: %w", err)
	}
	return nil
}

// adoptExistingDefaultVPC turns a default VPC built before claims existed into
// a completed claim. Duplicates left by the old race resolve to the oldest, so
// every node adopts the same one. Returns nil when the account has none.
func (s *VPCServiceImpl) adoptExistingDefaultVPC(ctx context.Context, accountID string, ids BootstrapIDs) (*defaultVPCClaim, error) {
	vpc, err := oldestAccountRecord(ctx, s.vpcKV, accountID, func(r *VPCRecord) (string, time.Time, bool) {
		return r.VpcId, r.CreatedAt, r.IsDefault
	})
	if err != nil || vpc == nil {
		return nil, err
	}
	subnet, err := oldestAccountRecord(ctx, s.subnetKV, accountID, func(r *SubnetRecord) (string, time.Time, bool) {
		return r.SubnetId, r.CreatedAt, r.IsDefault && r.VpcId == vpc.VpcId
	})
	if err != nil {
		return nil, err
	}

	claim := &defaultVPCClaim{
		VpcId:             vpc.VpcId,
		InternetGatewayId: cmp.Or(ids.IgwId, utils.GenerateResourceID("igw")),
		Cidr:              vpc.CidrBlock,
		VNI:               vpc.VNI,
		Complete:          true,
		CreatedAt:         time.Now(),
	}
	if subnet != nil {
		claim.SubnetId, claim.SubnetCidr = subnet.SubnetId, subnet.CidrBlock
	}
	slog.InfoContext(ctx, "Adopting existing default VPC", "vpcId", vpc.VpcId, "accountID", accountID)
	return claim, nil
}

// oldestAccountRecord returns the oldest of the account's records that match
// reports as wanted, breaking CreatedAt ties by ID. A record that exists but
// cannot be read fails the scan, since reading it as absent builds a duplicate.
func oldestAccountRecord[T any](ctx context.Context, kv jetstream.KeyValue, accountID string, match func(*T) (id string, created time.Time, wanted bool)) (*T, error) {
	keys, err := kv.Keys(ctx)
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", kv.Bucket(), err)
	}

	prefix := accountID + "."
	var best *T
	var bestID string
	var bestCreated time.Time
	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := kv.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s/%s: %w", kv.Bucket(), key, err)
		}
		record := new(T)
		if err := json.Unmarshal(entry.Value(), record); err != nil {
			continue
		}
		id, created, wanted := match(record)
		if !wanted {
			continue
		}
		if best == nil || created.Before(bestCreated) || (created.Equal(bestCreated) && id < bestID) {
			best, bestID, bestCreated = record, id, created
		}
	}
	return best, nil
}

// buildDefaultVPC creates whichever claimed records do not exist yet, then
// marks the claim complete. Records are written with Create, so only the caller
// that actually wrote one announces it to vpcd.
func (s *VPCServiceImpl) buildDefaultVPC(ctx context.Context, accountID string, claim *defaultVPCClaim) error {
	created, err := createRecord(ctx, s.vpcKV, utils.AccountKey(accountID, claim.VpcId), VPCRecord{
		VpcId:              claim.VpcId,
		CidrBlock:          claim.Cidr,
		State:              "available",
		IsDefault:          true,
		VNI:                claim.VNI,
		AZ:                 s.localAZ(),
		EnableDnsSupport:   true, // AWS default
		EnableDnsHostnames: true, // AWS default for default VPC
		Tags:               map[string]string{"Name": "default"},
		CreatedAt:          time.Now(),
	})
	if err != nil {
		return fmt.Errorf("store default VPC: %w", err)
	}
	if created {
		s.publishVPCEvent("vpc.create", claim.VpcId, claim.Cidr, claim.VNI)
	}

	az := "us-east-1a"
	if s.config != nil && s.config.AZ != "" {
		az = s.config.AZ
	}
	created, err = createRecord(ctx, s.subnetKV, utils.AccountKey(accountID, claim.SubnetId), SubnetRecord{
		SubnetId:            claim.SubnetId,
		VpcId:               claim.VpcId,
		CidrBlock:           claim.SubnetCidr,
		AvailabilityZone:    az,
		State:               "available",
		IsDefault:           true,
		MapPublicIpOnLaunch: !s.disableDefaultPublicIP, // AWS default subnets auto-assign public IPs (unless external mode has none)
		Tags:                map[string]string{"Name": "default"},
		CreatedAt:           time.Now(),
	})
	if err != nil {
		return fmt.Errorf("store default subnet: %w", err)
	}
	if created {
		s.publishSubnetEvent("vpc.create-subnet", claim.SubnetId, claim.VpcId, claim.SubnetCidr)
	}

	if s.rtbKV != nil {
		if _, err := s.writeMainRouteTable(ctx, accountID, claim.VpcId, claim.Cidr, claim.RouteTableId); err != nil {
			return fmt.Errorf("store default main route table: %w", err)
		}
	}

	sg, created, err := s.storeDefaultSecurityGroup(ctx, accountID, claim.VpcId, claim.GroupId)
	if err != nil {
		return err
	}
	if created {
		// Bootstrap runs before vpcd subscribes to vpc.create-sg, so this can
		// time out on first boot. vpcd's reconcile-sgs loop builds it from KV.
		if err := s.announceDefaultSecurityGroup(sg); err != nil {
			slog.WarnContext(ctx, "Default security group bootstrap deferred to vpcd reconciler",
				"vpcId", claim.VpcId, "accountID", accountID, "err", err)
		}
	}

	if _, err := kvutil.Update(ctx, s.defaultsKV, accountID, kvutil.CASConfig{}, func(c *defaultVPCClaim) (bool, error) {
		if c.Complete || c.VpcId != claim.VpcId {
			return false, nil
		}
		c.Complete = true
		return true, nil
	}); err != nil {
		return fmt.Errorf("mark default VPC claim complete: %w", err)
	}
	claim.Complete = true

	slog.InfoContext(ctx, "Default VPC ready",
		"vpcId", claim.VpcId, "subnetId", claim.SubnetId, "groupId", claim.GroupId,
		"routeTableId", claim.RouteTableId, "az", az, "accountID", accountID)
	return nil
}

// releaseDefaultVPCClaim retires the account's claim once its default VPC is
// deleted. Best effort: a claim left behind is retired by the next
// EnsureDefaultVPC, which finds its VPC gone.
func (s *VPCServiceImpl) releaseDefaultVPCClaim(ctx context.Context, accountID, vpcID string) {
	if s.defaultsKV == nil {
		return
	}
	claim, revision, err := s.readDefaultVPCClaim(ctx, accountID)
	if err != nil {
		slog.WarnContext(ctx, "DeleteVpc: default VPC claim unreadable", "accountID", accountID, "err", err)
		return
	}
	if claim == nil || claim.VpcId != vpcID {
		return
	}
	if err := s.defaultsKV.Delete(ctx, accountID, jetstream.LastRevision(revision)); err != nil {
		slog.WarnContext(ctx, "DeleteVpc: default VPC claim not released", "accountID", accountID, "vpcId", vpcID, "err", err)
	}
}

// createRecord stores value under key unless the key already exists.
func createRecord(ctx context.Context, kv jetstream.KeyValue, key string, value any) (created bool, err error) {
	data, err := json.Marshal(value)
	if err != nil {
		return false, err
	}
	if _, err := kv.Create(ctx, key, data); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
