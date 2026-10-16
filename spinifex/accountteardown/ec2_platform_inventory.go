package accountteardown

import (
	"context"
	"errors"
	"slices"
	"time"

	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

// ec2PlatformInventoryTimeout bounds one inventory request. The scan reads
// four stores for one account; a reply later than this is scan-incomplete.
const ec2PlatformInventoryTimeout = 30 * time.Second

// ec2PlatformInventory is EC2's read-only report of an account's key pairs,
// placement groups and launch templates. It never deletes, and a failure to
// obtain the report is returned as scan-incomplete kinds, never as clean.
type ec2PlatformInventory interface {
	PlatformInventory(ctx context.Context, accountID, correlationRef string) ec2v1.PlatformInventoryReply
}

// ec2PlatformInventoryClient reaches the EC2 platform-inventory contract over NATS.
type ec2PlatformInventoryClient struct {
	nc      *nats.Conn
	timeout time.Duration
}

var _ ec2PlatformInventory = (*ec2PlatformInventoryClient)(nil)

func newEC2PlatformInventoryClient(nc *nats.Conn) *ec2PlatformInventoryClient {
	return &ec2PlatformInventoryClient{nc: nc, timeout: ec2PlatformInventoryTimeout}
}

// PlatformInventory asks EC2 for the account's platform inventory. No
// responder, which is also how a daemon predating the subject answers, a
// timeout, an error reply or a malformed reply each make every kind incomplete.
func (c *ec2PlatformInventoryClient) PlatformInventory(ctx context.Context, accountID, correlationRef string) ec2v1.PlatformInventoryReply {
	reply, err := natsmsg.NATSRequest[ec2v1.PlatformInventoryReply](ctx, c.nc, ec2v1.PlatformInventorySubject,
		&ec2v1.PlatformInventoryRequest{AccountID: accountID, CorrelationRef: correlationRef}, c.timeout, accountID)
	if err != nil {
		return ec2v1.IncompletePlatformInventory(accountID, correlationRef, inventoryFailureReason(err))
	}
	return normalisePlatformInventory(*reply, accountID, correlationRef)
}

func inventoryFailureReason(err error) string {
	switch {
	case errors.Is(err, nats.ErrNoResponders):
		return ec2v1.PlatformReasonNoResponder
	case errors.Is(err, nats.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return ec2v1.PlatformReasonTimeout
	}
	if _, ok := awserrors.ResolveErrorCode(err); ok {
		return ec2v1.PlatformReasonErrorReply
	}
	return ec2v1.PlatformReasonTransportFailed
}

// normalisePlatformInventory refuses a reply for another account and turns a
// missing or repeated kind, or an unknown status, into scan-incomplete.
func normalisePlatformInventory(reply ec2v1.PlatformInventoryReply, accountID, correlationRef string) ec2v1.PlatformInventoryReply {
	if reply.AccountID != accountID {
		return ec2v1.IncompletePlatformInventory(accountID, correlationRef, ec2v1.PlatformReasonInvalidReply)
	}
	known := []string{ec2v1.PlatformStatusObservedClean, ec2v1.PlatformStatusRecordsFound, ec2v1.PlatformStatusScanIncomplete}
	byKind := make(map[string]ec2v1.PlatformKindReport, len(reply.Kinds))
	for _, report := range reply.Kinds {
		if !slices.Contains(known, report.Status) {
			report.Status = ec2v1.PlatformStatusScanIncomplete
			report.IncompleteReasons = append(report.IncompleteReasons, ec2v1.PlatformReasonInvalidReply)
		}
		if _, dup := byKind[report.Kind]; dup {
			report = ec2v1.PlatformKindReport{Kind: report.Kind, Status: ec2v1.PlatformStatusScanIncomplete,
				IncompleteReasons: []string{ec2v1.PlatformReasonInvalidReply}}
		}
		byKind[report.Kind] = report
	}
	reply.Kinds = nil
	for _, kind := range ec2v1.PlatformKinds() {
		report, ok := byKind[kind]
		if !ok {
			report = ec2v1.PlatformKindReport{Kind: kind, Status: ec2v1.PlatformStatusScanIncomplete,
				IncompleteReasons: []string{ec2v1.PlatformReasonKindNotReported}}
		}
		reply.Kinds = append(reply.Kinds, report)
	}
	return reply
}
