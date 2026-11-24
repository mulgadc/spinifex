package projection

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	ec2tgw "github.com/mulgadc/spinifex/spinifex/domains/ec2/tgw"
	"github.com/nats-io/nats.go/jetstream"
)

// The KV buckets whose changes alter Transit Gateway intent.
const (
	TransitGatewayBucket           = ec2tgw.KVBucketTransitGateways
	TransitGatewayAttachmentBucket = ec2tgw.KVBucketAttachments
)

// TransitGateway is one live Transit Gateway as network realises it.
type TransitGateway struct {
	ID         string
	AccountID  string
	Generation int64
	Pending    bool
	Available  bool
	// CleanupPending is set for deleting, and for deleted while the record
	// is still in the live bucket: its router must go.
	CleanupPending bool
}

// TransitGatewayAttachment is one live VPC attachment as network realises it.
// The link addresses are zero until the reconciler takes a link.
type TransitGatewayAttachment struct {
	ID               string
	AccountID        string
	TransitGatewayID string
	VpcID            string
	SubnetIDs        []string
	Generation       int64
	CreatedAt        time.Time
	VPCAddr          netip.Addr
	TGWAddr          netip.Addr
	Pending          bool
	Available        bool
	// CleanupPending is set for deleting, and for deleted or failed while
	// the record is still in the live bucket: its link must go.
	CleanupPending bool
}

// LoadTransitGateways reads every live Transit Gateway and attachment. A
// missing bucket is none; any other read failure is an error, never a partial list.
func LoadTransitGateways(ctx context.Context, js jetstream.JetStream) ([]TransitGateway, []TransitGatewayAttachment, error) {
	tgwKV, err := openOptional(ctx, js, TransitGatewayBucket)
	if err != nil || tgwKV == nil {
		return nil, nil, err
	}
	attKV, err := openOptional(ctx, js, TransitGatewayAttachmentBucket)
	if err != nil || attKV == nil {
		return nil, nil, err
	}
	gws, atts, err := ec2tgw.ListRecords(ctx, tgwKV, attKV)
	if err != nil {
		return nil, nil, err
	}
	outGWs := make([]TransitGateway, 0, len(gws))
	for i := range gws {
		rec := &gws[i]
		outGWs = append(outGWs, TransitGateway{
			ID: rec.ID, AccountID: rec.AccountID, Generation: rec.Generation,
			Pending:        rec.State == ec2tgw.StatePending,
			Available:      rec.State == ec2tgw.StateAvailable,
			CleanupPending: rec.State == ec2tgw.StateDeleting || rec.State == ec2tgw.StateDeleted,
		})
	}
	outAtts := make([]TransitGatewayAttachment, 0, len(atts))
	for i := range atts {
		outAtts = append(outAtts, attachment(&atts[i]))
	}
	return outGWs, outAtts, nil
}

func attachment(rec *ec2tgw.AttachmentRecord) TransitGatewayAttachment {
	a := TransitGatewayAttachment{
		ID: rec.ID, AccountID: rec.AccountID, TransitGatewayID: rec.TransitGatewayID, VpcID: rec.VpcID,
		SubnetIDs: rec.SubnetIDs, Generation: rec.Generation, CreatedAt: rec.CreatedAt,
		Pending:   rec.State == ec2tgw.StatePending,
		Available: rec.State == ec2tgw.StateAvailable,
		CleanupPending: rec.State == ec2tgw.StateDeleting || rec.State == ec2tgw.StateDeleted ||
			rec.State == ec2tgw.StateFailed,
	}
	if rec.LinkPrefix != "" {
		a.VPCAddr, a.TGWAddr, _ = ec2tgw.LinkAddresses(rec.LinkPrefix)
	}
	return a
}

// openOptional opens bucket, or returns nil when it was never created.
func openOptional(ctx context.Context, js jetstream.JetStream, bucket string) (jetstream.KeyValue, error) {
	kv, err := js.KeyValue(ctx, bucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", bucket, err)
	}
	return kv, nil
}
