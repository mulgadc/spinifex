// Package projection is EC2's authorised view for other domains. Network's
// reconciler reads VPC peering connections through it, never the store.
package projection

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

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

// LoadPeering reads every live connection. A missing peering bucket is none;
// any other read failure is an error, never a partial list.
func LoadPeering(ctx context.Context, js jetstream.JetStream) ([]PeeringConnection, error) {
	kv, err := js.KeyValue(ctx, PeeringBucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open peering bucket: %w", err)
	}
	recs, err := ec2vpcpeering.ListRecords(ctx, kv)
	if err != nil {
		return nil, err
	}
	out := make([]PeeringConnection, 0, len(recs))
	for i := range recs {
		out = append(out, connection(&recs[i]))
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
