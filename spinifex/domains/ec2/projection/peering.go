// Package projection is EC2's authorised view for other domains. Network's
// reconciler reads VPC peering connections through it, never the store.
package projection

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	ec2vpcpeering "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpcpeering"
	"github.com/nats-io/nats.go/jetstream"
)

// PeeringBucket is the KV bucket whose changes alter peering intent.
const PeeringBucket = ec2vpcpeering.KVBucketPeeringConnections

// PeeringConnection is one live connection as network realises it. A range or
// link address that does not parse is left zero.
type PeeringConnection struct {
	ID             string
	RequesterVPC   string
	AccepterVPC    string
	RequesterCIDR  netip.Prefix
	AccepterCIDR   netip.Prefix
	RequesterAddr  netip.Addr
	AccepterAddr   netip.Addr
	Generation     int64
	Linked         bool // provisioning or active: the link should exist
	Active         bool
	CleanupPending bool // deleted, and its link not yet confirmed gone
}

// PeeringSnapshot is every live connection and every VPC ID with a record,
// in any AZ. VPC presence comes from keys alone.
type PeeringSnapshot struct {
	Connections []PeeringConnection
	VPCIDs      map[string]struct{}
}

// LoadPeering reads the snapshot. A missing peering bucket is an empty
// snapshot; any other read failure is an error, never a partial snapshot.
func LoadPeering(ctx context.Context, js jetstream.JetStream) (PeeringSnapshot, error) {
	kv, err := js.KeyValue(ctx, PeeringBucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return PeeringSnapshot{VPCIDs: map[string]struct{}{}}, nil
	}
	if err != nil {
		return PeeringSnapshot{}, fmt.Errorf("open peering bucket: %w", err)
	}
	recs, err := ec2vpcpeering.ListRecords(ctx, kv)
	if err != nil {
		return PeeringSnapshot{}, err
	}
	vpcs, err := loadVPCIDs(ctx, js)
	if err != nil {
		return PeeringSnapshot{}, err
	}
	out := PeeringSnapshot{Connections: make([]PeeringConnection, 0, len(recs)), VPCIDs: vpcs}
	for i := range recs {
		out.Connections = append(out.Connections, connection(&recs[i]))
	}
	return out, nil
}

func connection(rec *ec2vpcpeering.PeeringRecord) PeeringConnection {
	c := PeeringConnection{
		ID: rec.ID, RequesterVPC: rec.RequesterVpcID, AccepterVPC: rec.AccepterVpcID,
		Generation:     rec.Generation,
		Linked:         rec.Status == ec2vpcpeering.StateProvisioning || rec.Status == ec2vpcpeering.StateActive,
		Active:         rec.Status == ec2vpcpeering.StateActive,
		CleanupPending: rec.CleanupPending,
	}
	c.RequesterCIDR, _ = netip.ParsePrefix(rec.RequesterCIDR)
	c.AccepterCIDR, _ = netip.ParsePrefix(rec.AccepterCIDR)
	if rec.LinkPrefix != "" {
		c.RequesterAddr, c.AccepterAddr, _ = ec2vpcpeering.LinkAddresses(rec.LinkPrefix)
	}
	return c
}

func loadVPCIDs(ctx context.Context, js jetstream.JetStream) (map[string]struct{}, error) {
	kv, err := js.KeyValue(ctx, ec2vpc.KVBucketVPCs)
	if err != nil {
		return nil, fmt.Errorf("open VPC bucket: %w", err)
	}
	keys, err := kv.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, fmt.Errorf("list VPC keys: %w", err)
	}
	out := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if _, id, ok := strings.Cut(key, "."); ok && strings.HasPrefix(id, "vpc-") {
			out[id] = struct{}{}
		}
	}
	return out, nil
}
