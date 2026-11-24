package tgw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	awstags "github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

var _ TransitGatewayService = (*ServiceImpl)(nil)

// Admission reports whether every node can carry Transit Gateway state. The
// caller holds done until its last write and calls it on every path.
type Admission func(ctx context.Context) (done func(), ok bool, err error)

// ServiceImpl serves the Transit Gateway actions from the Transit Gateway buckets.
type ServiceImpl struct {
	*Store

	region string
	// active admits writes and offered answers reads; nil admits nothing.
	active  Admission
	offered func(context.Context) (bool, error)
}

// NewServiceImpl wraps an open store; region is the formation's Region.
func NewServiceImpl(store *Store, region string) *ServiceImpl {
	return &ServiceImpl{Store: store, region: region}
}

// NewServiceImplWithNATS opens the buckets and returns the service.
func NewServiceImplWithNATS(ctx context.Context, natsConn *nats.Conn, region string) (*ServiceImpl, error) {
	js, err := jetstream.New(natsConn)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	store, err := OpenStore(ctx, js)
	if err != nil {
		return nil, err
	}
	slog.Info("Transit Gateway service initialized", "bucket", KVBucketTransitGateways)
	return NewServiceImpl(store, region), nil
}

// AccountStatus reads an account record's status. Implemented by
// domains/iam.AccountStatusReader.
type AccountStatus interface {
	AccountActive(ctx context.Context, accountID string) (bool, error)
	AccountExists(ctx context.Context, accountID string) (bool, error)
}

// SetAccountStatus installs the read a create makes after its record write.
// Until it is set, every create is refused.
func (s *ServiceImpl) SetAccountStatus(a AccountStatus) { s.accounts = a }

// accountActive refuses a create once account deletion has begun, or after it
// has removed the account record. It runs after the create's record write, so
// a create that passes it is seen by deletion's final reread.
func (s *ServiceImpl) accountActive(ctx context.Context, op, accountID string) error {
	if s.accounts == nil {
		return internalError(ctx, op, errors.New("no account status reader"))
	}
	ok, err := s.accounts.AccountActive(ctx, accountID)
	if err != nil {
		return internalError(ctx, op, err)
	}
	if !ok {
		return awserrors.Errorf(awserrors.ErrorAuthFailure, "The account is not active.")
	}
	return nil
}

// SetActivation installs the admission writes hold and the check reads use.
func (s *ServiceImpl) SetActivation(admit Admission, offered func(context.Context) (bool, error)) {
	s.active, s.offered = admit, offered
}

// admit holds the feature level for a write, or answers the action as not offered.
func (s *ServiceImpl) admit(ctx context.Context, op string) (func(), error) {
	if s.active == nil {
		return nil, notOffered()
	}
	done, ok, err := s.active(ctx)
	if err != nil {
		done()
		return nil, internalError(ctx, op, err)
	}
	if !ok {
		done()
		return nil, notOffered()
	}
	return done, nil
}

// readable answers whether reads are offered.
func (s *ServiceImpl) readable(ctx context.Context, op string) error {
	if s.offered == nil {
		return notOffered()
	}
	ok, err := s.offered(ctx)
	if err != nil {
		return internalError(ctx, op, err)
	}
	if !ok {
		return notOffered()
	}
	return nil
}

// internalError logs a store failure and answers it as AWS's server error.
func internalError(ctx context.Context, op string, err error) error {
	slog.ErrorContext(ctx, op+": store failure", "err", err)
	return errors.New(awserrors.ErrorServerInternal)
}

// enabled resolves an enable/disable option against its AWS default.
func enabled(param string, v *string, def string) (string, error) {
	switch s := aws.StringValue(v); s {
	case "":
		return def, nil
	case enable, disable:
		return s, nil
	default:
		return "", invalidValue(param, s)
	}
}

// AWS's private ASN ranges for a Transit Gateway.
func validASN(n int64) bool {
	return (n >= 64512 && n <= 65534) || (n >= 4200000000 && n <= 4294967294)
}

func transitGatewayOptions(in *ec2.TransitGatewayRequestOptions) (TransitGatewayOptions, error) {
	if in == nil {
		in = &ec2.TransitGatewayRequestOptions{}
	}
	opts := TransitGatewayOptions{AmazonSideAsn: 64512}
	if in.AmazonSideAsn != nil {
		if !validASN(*in.AmazonSideAsn) {
			return opts, invalidValue("AmazonSideAsn", strconv.FormatInt(*in.AmazonSideAsn, 10))
		}
		opts.AmazonSideAsn = *in.AmazonSideAsn
	}
	for _, p := range []struct {
		name string
		v    *string
	}{
		{"DefaultRouteTableAssociation", in.DefaultRouteTableAssociation},
		{"DefaultRouteTablePropagation", in.DefaultRouteTablePropagation},
		{"MulticastSupport", in.MulticastSupport},
	} {
		def := enable
		if p.name == "MulticastSupport" {
			def = disable
		}
		v, err := enabled(p.name, p.v, def)
		if err != nil {
			return opts, err
		}
		if v != def {
			return opts, unsupportedValue(p.name, v)
		}
	}
	if len(in.TransitGatewayCidrBlocks) > 0 {
		return opts, unsupportedValue("TransitGatewayCidrBlocks", aws.StringValue(in.TransitGatewayCidrBlocks[0]))
	}
	var err error
	if opts.AutoAcceptSharedAttachments, err = enabled("AutoAcceptSharedAttachments", in.AutoAcceptSharedAttachments, disable); err != nil {
		return opts, err
	}
	if opts.DnsSupport, err = enabled("DnsSupport", in.DnsSupport, enable); err != nil {
		return opts, err
	}
	if opts.VpnEcmpSupport, err = enabled("VpnEcmpSupport", in.VpnEcmpSupport, enable); err != nil {
		return opts, err
	}
	if opts.SecurityGroupReferencingSupport, err = enabled("SecurityGroupReferencingSupport", in.SecurityGroupReferencingSupport, disable); err != nil {
		return opts, err
	}
	return opts, nil
}

// CreateTransitGateway writes a pending Transit Gateway; the reconciler makes
// it available.
func (s *ServiceImpl) CreateTransitGateway(ctx context.Context, input *ec2.CreateTransitGatewayInput, accountID string) (*ec2.CreateTransitGatewayOutput, error) {
	const op = "CreateTransitGateway"
	done, err := s.admit(ctx, op)
	if err != nil {
		return nil, err
	}
	defer done()
	opts, err := transitGatewayOptions(input.Options)
	if err != nil {
		return nil, err
	}
	now := s.now()
	rec := &TransitGatewayRecord{
		ID:          awsidentifiers.GenerateResourceID("tgw"),
		AccountID:   accountID,
		Description: aws.StringValue(input.Description),
		State:       StatePending,
		Options:     opts,
		Generation:  1,
		Tags:        awstags.Extract(input.TagSpecifications, ec2.ResourceTypeTransitGateway),
		CreatedAt:   now,
	}
	slot, slotRev, err := s.takeQuotaSlot(ctx, rec)
	if errors.Is(err, errClaimHeld) {
		return nil, awserrors.Errorf(awserrors.ErrorResourceLimitExceeded,
			"You have reached the limit of %d transit gateways for this account.", accountLimit)
	}
	if err != nil {
		return nil, internalError(ctx, op, err)
	}
	acct := acctKey(accountID, rec.ID)
	acctRev, err := putJSON(ctx, s.tgws, acct, indexEntry{AccountID: accountID, CreatedAt: now})
	if err != nil {
		return nil, s.undoTransitGateway(ctx, rec, internalError(ctx, op, err))
	}
	if _, err := writeJSON(ctx, s.tgws, recordKey(accountID, rec.ID), rec, 0); err != nil {
		return nil, s.undoTransitGateway(ctx, rec, internalError(ctx, op, err))
	}
	if err := touchIndexes(ctx, s.tgws, map[string]uint64{acct: acctRev}); err != nil {
		return nil, s.undoTransitGateway(ctx, rec, internalError(ctx, op, err))
	}
	// A create judged stale during a pause before its record write has lost
	// its slot; the rewrite at the revision taken shows that.
	if err := touchAt(ctx, s.tgws, slot, slotRev); err != nil {
		return nil, s.undoTransitGateway(ctx, rec, internalError(ctx, op, fmt.Errorf("quota slot %s: %w", slot, err)))
	}
	if err := s.accountActive(ctx, op, accountID); err != nil {
		return nil, s.undoTransitGateway(ctx, rec, err)
	}
	s.projectTags(ctx, accountID, rec.ID, rec.Tags)
	return &ec2.CreateTransitGatewayOutput{TransitGateway: s.transitGatewayToEC2(rec, true)}, nil
}

// DeleteTransitGateway writes the Transit Gateway as deleting at the revision
// it checked for attachments. That write is the fence admission rewrites against.
func (s *ServiceImpl) DeleteTransitGateway(ctx context.Context, input *ec2.DeleteTransitGatewayInput, accountID string) (*ec2.DeleteTransitGatewayOutput, error) {
	const op = "DeleteTransitGateway"
	done, err := s.admit(ctx, op)
	if err != nil {
		return nil, err
	}
	defer done()
	id := aws.StringValue(input.TransitGatewayId)
	if err := validTransitGatewayID(id); err != nil {
		return nil, err
	}
	for range moveAttempts {
		rec, rev, err := s.transitGateway(ctx, accountID, id)
		if err != nil {
			return nil, internalError(ctx, op, err)
		}
		if rec == nil {
			term, err := s.terminalTransitGateway(ctx, accountID, id)
			if err != nil {
				return nil, internalError(ctx, op, err)
			}
			if term == nil {
				return nil, transitGatewayNotFound(id)
			}
			return &ec2.DeleteTransitGatewayOutput{TransitGateway: s.transitGatewayToEC2(term, false)}, nil
		}
		// A deleted record still in the live bucket is a move the sweep finishes.
		if rec.State == StateDeleting || rec.State == StateDeleted {
			return &ec2.DeleteTransitGatewayOutput{TransitGateway: s.transitGatewayToEC2(rec, false)}, nil
		}
		blocking, err := s.liveAttachmentsUnder(ctx, "tgw."+id+".")
		if err != nil {
			return nil, internalError(ctx, op, err)
		}
		if len(blocking) > 0 {
			slices.Sort(blocking)
			return nil, awserrors.Errorf(awserrors.ErrorIncorrectState,
				"%s has non-deleted VPC Attachments: %s.", id, strings.Join(blocking, ", "))
		}
		rec.State = StateDeleting
		rec.Generation++
		if _, err := writeJSON(ctx, s.tgws, recordKey(accountID, id), rec, rev); err != nil {
			if errors.Is(err, errConflict) {
				continue
			}
			return nil, internalError(ctx, op, err)
		}
		return &ec2.DeleteTransitGatewayOutput{TransitGateway: s.transitGatewayToEC2(rec, false)}, nil
	}
	return nil, internalError(ctx, op, fmt.Errorf("delete %s: %w", id, errConflict))
}

func attachmentOptions(in *ec2.CreateTransitGatewayVpcAttachmentRequestOptions) (AttachmentOptions, error) {
	if in == nil {
		in = &ec2.CreateTransitGatewayVpcAttachmentRequestOptions{}
	}
	var opts AttachmentOptions
	for _, p := range []struct {
		name string
		v    *string
	}{{"ApplianceModeSupport", in.ApplianceModeSupport}, {"Ipv6Support", in.Ipv6Support}} {
		v, err := enabled(p.name, p.v, disable)
		if err != nil {
			return opts, err
		}
		if v != disable {
			return opts, unsupportedValue(p.name, v)
		}
	}
	var err error
	if opts.DnsSupport, err = enabled("DnsSupport", in.DnsSupport, enable); err != nil {
		return opts, err
	}
	if opts.SecurityGroupReferencingSupport, err = enabled("SecurityGroupReferencingSupport", in.SecurityGroupReferencingSupport, enable); err != nil {
		return opts, err
	}
	return opts, nil
}

// parentRevs are the revisions admission read and rewrites its parents at.
type parentRevs struct {
	tgw     uint64
	vpc     uint64
	subnets map[string]uint64
}

// CreateTransitGatewayVpcAttachment writes a pending attachment, then rewrites
// each parent at the revision it checked, so a parent delete that checked the
// same revision loses. A parent that changed is re-read and judged again.
func (s *ServiceImpl) CreateTransitGatewayVpcAttachment(ctx context.Context, input *ec2.CreateTransitGatewayVpcAttachmentInput, accountID string) (*ec2.CreateTransitGatewayVpcAttachmentOutput, error) {
	const op = "CreateTransitGatewayVpcAttachment"
	done, err := s.admit(ctx, op)
	if err != nil {
		return nil, err
	}
	defer done()
	tgwID, vpcID := aws.StringValue(input.TransitGatewayId), aws.StringValue(input.VpcId)
	subnetIDs := awsidentifiers.Distinct(input.SubnetIds)
	if err := validTransitGatewayID(tgwID); err != nil {
		return nil, err
	}
	if len(subnetIDs) == 0 {
		return nil, awserrors.Errorf(awserrors.ErrorMissingParameter,
			"The request must include the SubnetIds parameter. Add the required parameter and retry the request.")
	}
	opts, err := attachmentOptions(input.Options)
	if err != nil {
		return nil, err
	}
	revs, err := s.checkParents(ctx, accountID, tgwID, vpcID, subnetIDs)
	if err != nil {
		return nil, err
	}

	rec := &AttachmentRecord{
		ID:               awsidentifiers.GenerateResourceID("tgw-attach"),
		AccountID:        accountID,
		TransitGatewayID: tgwID,
		VpcID:            vpcID,
		SubnetIDs:        subnetIDs,
		Options:          opts,
		State:            StatePending,
		Generation:       1,
		Tags:             awstags.Extract(input.TagSpecifications, ec2.ResourceTypeTransitGatewayAttachment),
		CreatedAt:        s.now(),
	}
	duplicate := awserrors.Errorf(awserrors.ErrorDuplicateTransitGatewayAttachment,
		"%s has non-deleted Transit Gateway Attachments with same VPC ID.", tgwID)
	pair := pairKey(tgwID, vpcID)
	pairRev, err := s.takeClaim(ctx, s.atts, pair, s.attachmentClaim(rec), s.attachmentHolderStale)
	if errors.Is(err, errClaimHeld) {
		return nil, duplicate
	}
	if err != nil {
		return nil, internalError(ctx, op, err)
	}
	indexRevs, err := s.writeAttachmentIndexes(ctx, rec)
	if err != nil {
		return nil, s.undoAdmission(ctx, op, rec, internalError(ctx, op, err))
	}
	if _, err := writeJSON(ctx, s.atts, recordKey(accountID, rec.ID), rec, 0); err != nil {
		return nil, s.undoAdmission(ctx, op, rec, internalError(ctx, op, err))
	}
	if err := touchIndexes(ctx, s.atts, indexRevs); err != nil {
		return nil, s.undoAdmission(ctx, op, rec, internalError(ctx, op, err))
	}
	// A newer admission took the pair over while this one paused before its
	// record write.
	if err := touchAt(ctx, s.atts, pair, pairRev); err != nil {
		if errors.Is(err, errConflict) {
			return nil, s.undoAdmission(ctx, op, rec, duplicate)
		}
		return nil, s.undoAdmission(ctx, op, rec, internalError(ctx, op, err))
	}
	if err := s.fenceParents(ctx, rec, revs); err != nil {
		if _, ok := awserrors.ResolveErrorCode(err); !ok {
			err = internalError(ctx, op, err)
		}
		return nil, s.undoAdmission(ctx, op, rec, err)
	}
	if err := s.accountActive(ctx, op, accountID); err != nil {
		return nil, s.undoAdmission(ctx, op, rec, err)
	}
	s.projectTags(ctx, accountID, rec.ID, rec.Tags)
	return &ec2.CreateTransitGatewayVpcAttachmentOutput{TransitGatewayVpcAttachment: attachmentToEC2(rec)}, nil
}

// checkParents reads and judges the Transit Gateway, VPC and subnets, and
// returns the revisions it judged.
func (s *ServiceImpl) checkParents(ctx context.Context, accountID, tgwID, vpcID string, subnetIDs []string) (parentRevs, error) {
	revs := parentRevs{subnets: map[string]uint64{}}
	var err error
	if revs.tgw, err = s.checkTransitGateway(ctx, accountID, tgwID); err != nil {
		return revs, err
	}
	if revs.vpc, err = s.checkVPC(ctx, accountID, vpcID); err != nil {
		return revs, err
	}
	zones := map[string]bool{}
	for _, id := range subnetIDs {
		sub, rev, err := s.checkSubnet(ctx, accountID, vpcID, id)
		if err != nil {
			return revs, err
		}
		if zones[sub.AvailabilityZone] {
			return revs, awserrors.Errorf(awserrors.ErrorDuplicateSubnetsInSameZone, "Duplicate Subnets for same AZ")
		}
		zones[sub.AvailabilityZone] = true
		revs.subnets[id] = rev
	}
	return revs, nil
}

func (s *ServiceImpl) checkTransitGateway(ctx context.Context, accountID, id string) (uint64, error) {
	rec, rev, err := s.transitGateway(ctx, accountID, id)
	if err != nil {
		return 0, internalError(ctx, "CreateTransitGatewayVpcAttachment", err)
	}
	if rec == nil {
		term, err := s.terminalTransitGateway(ctx, accountID, id)
		if err != nil {
			return 0, internalError(ctx, "CreateTransitGatewayVpcAttachment", err)
		}
		if term == nil {
			return 0, transitGatewayNotFound(id)
		}
		return 0, incorrectState(id)
	}
	if rec.State != StateAvailable {
		return 0, incorrectState(id)
	}
	return rev, nil
}

func (s *ServiceImpl) checkVPC(ctx context.Context, accountID, id string) (uint64, error) {
	vpc, rev, err := s.parents.VPC(ctx, accountID, id)
	if err != nil {
		return 0, internalError(ctx, "CreateTransitGatewayVpcAttachment", err)
	}
	if vpc == nil || vpc.Deleting {
		return 0, awserrors.IDNotFound(awserrors.ErrorInvalidVpcIDNotFound, "vpc", id)
	}
	// AWS refuses such a CIDR at CreateVpc; a VPC created before that check
	// could hold one, and its subnets could overlap a link.
	if overlapsLinkLocal(vpc.CidrBlock) {
		return 0, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"The VPC %s has the CIDR %s inside 169.254.0.0/16, which a Transit Gateway attachment cannot use.", id, vpc.CidrBlock)
	}
	return rev, nil
}

func (s *ServiceImpl) checkSubnet(ctx context.Context, accountID, vpcID, id string) (*ec2vpc.SubnetRecord, uint64, error) {
	sub, rev, err := s.parents.Subnet(ctx, accountID, id)
	if err != nil {
		return nil, 0, internalError(ctx, "CreateTransitGatewayVpcAttachment", err)
	}
	if sub == nil || sub.Deleting {
		return nil, 0, awserrors.IDNotFound(awserrors.ErrorInvalidSubnetIDNotFound, "subnet", id)
	}
	if sub.VpcId != vpcID {
		return nil, 0, awserrors.Errorf(awserrors.ErrorInvalidSubnetIDNotFound, "Subnet %s was deleted or does not exist.", id)
	}
	return sub, rev, nil
}

// fenceParents rewrites each parent at the revision admission judged. A
// rewrite that loses re-reads the parent: one now absent, marked or not
// available answers its AWS error; otherwise the rewrite retries.
func (s *ServiceImpl) fenceParents(ctx context.Context, rec *AttachmentRecord, revs parentRevs) error {
	acct := rec.AccountID
	if err := s.fenceOne(ctx, revs.tgw,
		func(rev uint64) error { return touchAt(ctx, s.tgws, recordKey(acct, rec.TransitGatewayID), rev) },
		func() (uint64, error) { return s.checkTransitGateway(ctx, acct, rec.TransitGatewayID) }); err != nil {
		return err
	}
	if err := s.fenceOne(ctx, revs.vpc,
		func(rev uint64) error { return s.parents.TouchVPC(ctx, acct, rec.VpcID, rev) },
		func() (uint64, error) { return s.checkVPC(ctx, acct, rec.VpcID) }); err != nil {
		return err
	}
	for _, id := range rec.SubnetIDs {
		if err := s.fenceOne(ctx, revs.subnets[id],
			func(rev uint64) error { return s.parents.TouchSubnet(ctx, acct, id, rev) },
			func() (uint64, error) {
				_, rev, err := s.checkSubnet(ctx, acct, rec.VpcID, id)
				return rev, err
			}); err != nil {
			return err
		}
	}
	return nil
}

func (s *ServiceImpl) fenceOne(ctx context.Context, rev uint64, touch func(uint64) error, recheck func() (uint64, error)) error {
	for range moveAttempts {
		err := touch(rev)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errConflict) && !errors.Is(err, ec2vpc.ErrParentChanged) {
			return err
		}
		if rev, err = recheck(); err != nil {
			return err
		}
	}
	return fmt.Errorf("fence parent: %w", errConflict)
}

// undoAdmission removes what admission wrote, record first so no record is
// left without the keys that make a parent delete see it, and returns cause.
func (s *ServiceImpl) undoAdmission(ctx context.Context, op string, rec *AttachmentRecord, cause error) error {
	if err := deleteKey(ctx, s.atts, recordKey(rec.AccountID, rec.ID)); err != nil {
		slog.ErrorContext(ctx, op+": undo failed; the sweep removes the keys", "transitGatewayAttachmentId", rec.ID, "err", err)
		return cause
	}
	if err := s.releaseAttachment(ctx, rec); err != nil {
		slog.ErrorContext(ctx, op+": undo failed; the sweep removes the keys", "transitGatewayAttachmentId", rec.ID, "err", err)
	}
	return cause
}

// undoTransitGateway removes what a create wrote and returns cause.
func (s *ServiceImpl) undoTransitGateway(ctx context.Context, rec *TransitGatewayRecord, cause error) error {
	if err := s.removeTransitGateway(ctx, rec); err != nil {
		slog.ErrorContext(ctx, "CreateTransitGateway: undo failed; the sweep removes the keys", "transitGatewayId", rec.ID, "err", err)
	}
	return cause
}

// DeleteTransitGatewayVpcAttachment writes the attachment as deleting; the
// reconciler tears it down and moves it to the terminal bucket.
func (s *ServiceImpl) DeleteTransitGatewayVpcAttachment(ctx context.Context, input *ec2.DeleteTransitGatewayVpcAttachmentInput, accountID string) (*ec2.DeleteTransitGatewayVpcAttachmentOutput, error) {
	const op = "DeleteTransitGatewayVpcAttachment"
	done, err := s.admit(ctx, op)
	if err != nil {
		return nil, err
	}
	defer done()
	id := aws.StringValue(input.TransitGatewayAttachmentId)
	if err := validAttachmentID(id); err != nil {
		return nil, err
	}
	for range moveAttempts {
		rec, rev, err := s.attachment(ctx, accountID, id)
		if err != nil {
			return nil, internalError(ctx, op, err)
		}
		if rec == nil {
			term, err := s.terminalAttachment(ctx, accountID, id)
			if err != nil {
				return nil, internalError(ctx, op, err)
			}
			if term == nil {
				return nil, vpcAttachmentNotFound(id)
			}
			return &ec2.DeleteTransitGatewayVpcAttachmentOutput{TransitGatewayVpcAttachment: attachmentDeleteShape(term)}, nil
		}
		if rec.State == StateDeleting || !rec.isLive() {
			return &ec2.DeleteTransitGatewayVpcAttachmentOutput{TransitGatewayVpcAttachment: attachmentDeleteShape(rec)}, nil
		}
		rec.State = StateDeleting
		rec.Generation++
		if _, err := writeJSON(ctx, s.atts, recordKey(accountID, id), rec, rev); err != nil {
			if errors.Is(err, errConflict) {
				continue
			}
			return nil, internalError(ctx, op, err)
		}
		return &ec2.DeleteTransitGatewayVpcAttachmentOutput{TransitGatewayVpcAttachment: attachmentDeleteShape(rec)}, nil
	}
	return nil, internalError(ctx, op, fmt.Errorf("delete %s: %w", id, errConflict))
}
