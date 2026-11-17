package vpcpeering

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	awstags "github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

var _ VpcPeeringService = (*ServiceImpl)(nil)

// ServiceImpl serves the peering actions from the peering buckets.
type ServiceImpl struct {
	*Store

	region string
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
	slog.Info("VPC peering service initialized", "bucket", KVBucketPeeringConnections)
	return NewServiceImpl(store, region), nil
}

// internalError logs a store failure and answers it as AWS's server error.
func internalError(ctx context.Context, op string, err error) error {
	slog.ErrorContext(ctx, op+": store failure", "err", err)
	return errors.New(awserrors.ErrorServerInternal)
}

// vpcIDMalformed is AWS's answer to a VpcId or PeerVpcId not of the vpc- form.
func vpcIDMalformed(id string) error {
	suffix, ok := strings.CutPrefix(id, "vpc-")
	if ok && (len(suffix) == 8 || len(suffix) == 17) && ec2vpc.IsLowerHex(suffix) {
		return nil
	}
	return awserrors.Errorf(awserrors.ErrorInvalidVpcIdMalformed, "Invalid id: \"%s\" (expecting \"vpc-...\")", id)
}

// CreateVpcPeeringConnection requests a connection from the caller's VpcId to
// PeerVpcId. Conditions AWS reports asynchronously produce a failed
// connection, written straight to the terminal bucket.
func (s *ServiceImpl) CreateVpcPeeringConnection(ctx context.Context, input *ec2.CreateVpcPeeringConnectionInput, accountID string) (*ec2.CreateVpcPeeringConnectionOutput, error) {
	vpcID, peerVpcID := aws.StringValue(input.VpcId), aws.StringValue(input.PeerVpcId)
	if vpcID == "" {
		return nil, awserrors.Errorf(awserrors.ErrorMissingParameter, "The request must contain the parameter vpcId")
	}
	if peerVpcID == "" {
		return nil, awserrors.Errorf(awserrors.ErrorMissingParameter, "The request must contain the parameter peerVpcId")
	}
	for _, id := range []string{vpcID, peerVpcID} {
		if err := vpcIDMalformed(id); err != nil {
			return nil, err
		}
	}
	if r := aws.StringValue(input.PeerRegion); r != "" && r != s.region {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"Invalid value '%s' for peerRegion. Inter-Region VPC peering is not supported; the peer VPC must be in %s.", r, s.region)
	}
	peerOwner := accountID
	if o := aws.StringValue(input.PeerOwnerId); o != "" && o != accountID {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"Invalid value '%s' for peerOwnerId. Peering with a VPC in another account is not supported.", o)
	}

	vpc, err := s.vpcRecord(ctx, accountID, vpcID)
	if err != nil {
		return nil, internalError(ctx, "CreateVpcPeeringConnection", err)
	}
	// AWS calls an unknown well-formed requester VPC malformed.
	if vpc == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidVpcIdMalformed, "The vpc ID '%s' is malformed", vpcID)
	}
	if peerVpcID == vpcID {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"Invalid value '%s' for peerVpcId. The value of peerVpcId needs to be different to vpcId.", peerVpcID)
	}
	peer, err := s.vpcRecord(ctx, peerOwner, peerVpcID)
	if err != nil {
		return nil, internalError(ctx, "CreateVpcPeeringConnection", err)
	}
	if peer == nil {
		return nil, awserrors.IDNotFound(awserrors.ErrorInvalidVpcIDNotFound, "vpc", peerVpcID)
	}

	if existing, err := s.openRequest(ctx, vpcID, peerVpcID); err != nil {
		return nil, internalError(ctx, "CreateVpcPeeringConnection", err)
	} else if existing != nil {
		return &ec2.CreateVpcPeeringConnectionOutput{VpcPeeringConnection: existingToEC2(existing, accountID)}, nil
	}

	now := s.now()
	rec := &PeeringRecord{
		ID:                 awsidentifiers.GenerateResourceID("pcx"),
		RequesterAccountID: accountID,
		RequesterVpcID:     vpcID,
		RequesterCIDR:      vpc.CidrBlock,
		AccepterAccountID:  peerOwner,
		AccepterVpcID:      peerVpcID,
		AccepterCIDR:       peer.CidrBlock,
		Region:             s.region,
		ExpirationTime:     now.Add(requestLifetime),
		Generation:         1,
		Tags:               map[string]map[string]string{},
		CreatedAt:          now,
	}
	if tags := awstags.Extract(input.TagSpecifications, ec2.ResourceTypeVpcPeeringConnection); len(tags) > 0 {
		rec.Tags[accountID] = tags
	}

	// A VPC with no AZ home predates homes and matches every AZ.
	homesDiffer := vpc.AZ != "" && peer.AZ != "" && vpc.AZ != peer.AZ
	overlap := cidrsOverlap(vpc.CidrBlock, peer.CidrBlock)
	if overlap || homesDiffer {
		rec.Status, rec.StatusMessage = StateFailed, failedMessage
		rec.TerminalAt, rec.TerminatedBy = now, ""
		if err := s.writeTerminal(ctx, rec); err != nil {
			return nil, internalError(ctx, "CreateVpcPeeringConnection", err)
		}
		slog.InfoContext(ctx, "CreateVpcPeeringConnection failed the connection", "vpcPeeringConnectionId", rec.ID,
			"vpcId", vpcID, "peerVpcId", peerVpcID, "cidrOverlap", overlap, "azHomesDiffer", homesDiffer, "accountID", accountID)
		return &ec2.CreateVpcPeeringConnectionOutput{
			VpcPeeringConnection: toEC2(rec, accountID, StateInitiatingRequest, initiatingMessage(peerOwner), shapeFailedCreate),
		}, nil
	}

	if err := s.checkLimits(ctx, vpcID, peerVpcID); err != nil {
		return nil, err
	}

	rec.Status, rec.StatusMessage = StatePendingAcceptance, pendingMessage(peerOwner)
	if err := s.writeIndexes(ctx, rec); err != nil {
		return nil, internalError(ctx, "CreateVpcPeeringConnection", err)
	}
	recRev, err := writeJSON(ctx, s.live, recordKey(rec.ID), rec, 0)
	if err != nil {
		return nil, internalError(ctx, "CreateVpcPeeringConnection", err)
	}
	holder, err := s.takeReqClaim(ctx, rec)
	if err != nil {
		return nil, internalError(ctx, "CreateVpcPeeringConnection", err)
	}
	if holder != rec.ID {
		// A concurrent create in the same direction won: drop this one and
		// answer with the winner, as a repeated request would.
		if err := s.discardUnclaimed(ctx, rec); err != nil {
			slog.WarnContext(ctx, "CreateVpcPeeringConnection: discard lost request failed; the sweep removes it", "vpcPeeringConnectionId", rec.ID, "err", err)
		}
		winner, err := s.load(ctx, holder)
		if err != nil {
			return nil, internalError(ctx, "CreateVpcPeeringConnection", err)
		}
		if winner == nil {
			return nil, internalError(ctx, "CreateVpcPeeringConnection", fmt.Errorf("winning request %s vanished", holder))
		}
		return &ec2.CreateVpcPeeringConnectionOutput{VpcPeeringConnection: existingToEC2(winner, accountID)}, nil
	}
	if live, err := s.confirmCreated(ctx, rec, recRev); err != nil || !live {
		if err == nil {
			err = fmt.Errorf("request %s was swept before its claim", rec.ID)
			if derr := s.deleteIndexes(ctx, rec); derr != nil {
				slog.WarnContext(ctx, "CreateVpcPeeringConnection: discard swept request failed; the sweep removes it", "vpcPeeringConnectionId", rec.ID, "err", derr)
			}
		}
		return nil, internalError(ctx, "CreateVpcPeeringConnection", err)
	}

	slog.InfoContext(ctx, "CreateVpcPeeringConnection completed", "vpcPeeringConnectionId", rec.ID,
		"vpcId", vpcID, "peerVpcId", peerVpcID, "accountID", accountID)
	return &ec2.CreateVpcPeeringConnectionOutput{
		VpcPeeringConnection: toEC2(rec, accountID, StateInitiatingRequest, initiatingMessage(peerOwner), shapeRequest),
	}, nil
}

// openRequest returns the valid pending, provisioning or active connection
// the directed claim names, or nil.
func (s *ServiceImpl) openRequest(ctx context.Context, vpcID, peerVpcID string) (*view, error) {
	held, _, err := getJSON[claim](ctx, s.live, reqKey(vpcID, peerVpcID))
	if err != nil || held == nil {
		return nil, err
	}
	v, err := s.load(ctx, held.PcxID)
	if err != nil || v == nil || !v.isOpen() {
		return nil, err
	}
	return v, nil
}

// discardUnclaimed removes a record that lost its request claim, and its keys.
func (s *Store) discardUnclaimed(ctx context.Context, rec *PeeringRecord) error {
	if err := deleteKey(ctx, s.live, recordKey(rec.ID)); err != nil {
		return err
	}
	return s.deleteIndexes(ctx, rec)
}

// vpcConnections returns the live, non-terminal connections in vpcID's index.
func (s *Store) vpcConnections(ctx context.Context, vpcID string) ([]*view, error) {
	ids, err := listIndex(ctx, s.live, "vpc."+vpcID+".")
	if err != nil {
		return nil, err
	}
	var out []*view
	for _, id := range ids {
		v, err := s.load(ctx, id)
		if err != nil {
			return nil, err
		}
		if v != nil && v.isOpen() {
			out = append(out, v)
		}
	}
	return out, nil
}

// checkLimits applies the per-VPC request and connection limits. The count
// is not atomic across different pairs, as the security-group limit is not.
func (s *ServiceImpl) checkLimits(ctx context.Context, vpcID, peerVpcID string) error {
	for _, id := range []string{vpcID, peerVpcID} {
		conns, err := s.vpcConnections(ctx, id)
		if err != nil {
			return internalError(ctx, "CreateVpcPeeringConnection", err)
		}
		if id == vpcID {
			outstanding := 0
			for _, v := range conns {
				if v.status == StatePendingAcceptance && v.rec.RequesterVpcID == vpcID {
					outstanding++
				}
			}
			if outstanding >= outstandingLimit {
				return errors.New(awserrors.ErrorOutstandingVpcPeeringConnectionLimitExceeded)
			}
		}
		if len(conns) >= perVpcLimit {
			return errors.New(awserrors.ErrorVpcPeeringConnectionsPerVpcLimitExceeded)
		}
	}
	return nil
}

// visible loads id as viewer sees it, or nil.
func (s *Store) visible(ctx context.Context, id, viewer string) (*view, error) {
	v, err := s.load(ctx, id)
	if err != nil || v == nil || !v.visibleTo(viewer, s.now()) {
		return nil, err
	}
	return v, nil
}

// AcceptVpcPeeringConnection accepts a pending request as its accepter. It
// takes the pair and link claims, records provisioning and returns; the
// reconciler realises the link and marks it active.
func (s *ServiceImpl) AcceptVpcPeeringConnection(ctx context.Context, input *ec2.AcceptVpcPeeringConnectionInput, accountID string) (*ec2.AcceptVpcPeeringConnectionOutput, error) {
	id := aws.StringValue(input.VpcPeeringConnectionId)
	for range moveAttempts {
		v, err := s.visible(ctx, id, accountID)
		if err != nil {
			return nil, internalError(ctx, "AcceptVpcPeeringConnection", err)
		}
		if v == nil || v.rec.AccepterAccountID != accountID {
			return nil, notFoundError(id)
		}
		if v.isLinkedAlive() {
			return &ec2.AcceptVpcPeeringConnectionOutput{VpcPeeringConnection: viewToEC2(v, accountID)}, nil
		}
		if v.status != StatePendingAcceptance {
			return nil, transitionError(id, v.status, StateProvisioning)
		}

		pk := pairKey(v.rec.RequesterVpcID, v.rec.AccepterVpcID)
		if err := s.takeOwnedClaim(ctx, pk, id, pairClaimStale); err != nil {
			if errors.Is(err, errClaimHeld) {
				return nil, alreadyExistsError(id)
			}
			return nil, internalError(ctx, "AcceptVpcPeeringConnection", err)
		}
		// A failed Accept keeps its claims: a concurrent Accept of the same ID
		// shares them, and they go stale once no linked record carries them.
		prefix, err := s.takeLinkSlot(ctx, id)
		if err != nil {
			return nil, internalError(ctx, "AcceptVpcPeeringConnection", err)
		}

		rec := v.rec
		rec.Status, rec.StatusMessage = StateProvisioning, provisioningMessage
		rec.ExpirationTime = time.Time{}
		rec.Generation++
		rec.LinkPrefix = prefix
		if _, err := writeJSON(ctx, s.live, recordKey(id), &rec, v.rev); err != nil {
			if errors.Is(err, errConflict) {
				continue
			}
			return nil, internalError(ctx, "AcceptVpcPeeringConnection", err)
		}
		slog.InfoContext(ctx, "AcceptVpcPeeringConnection recorded", "vpcPeeringConnectionId", id,
			"linkPrefix", prefix, "generation", rec.Generation, "accountID", accountID)
		return &ec2.AcceptVpcPeeringConnectionOutput{
			VpcPeeringConnection: toEC2(&rec, accountID, StateProvisioning, provisioningMessage, shapeLinked),
		}, nil
	}
	return nil, internalError(ctx, "AcceptVpcPeeringConnection", fmt.Errorf("accept %s: %w", id, errConflict))
}

// RejectVpcPeeringConnection rejects a pending request as its accepter.
func (s *ServiceImpl) RejectVpcPeeringConnection(ctx context.Context, input *ec2.RejectVpcPeeringConnectionInput, accountID string) (*ec2.RejectVpcPeeringConnectionOutput, error) {
	id := aws.StringValue(input.VpcPeeringConnectionId)
	for range moveAttempts {
		v, err := s.visible(ctx, id, accountID)
		if err != nil {
			return nil, internalError(ctx, "RejectVpcPeeringConnection", err)
		}
		if v == nil || v.rec.AccepterAccountID != accountID {
			return nil, notFoundError(id)
		}
		if v.status != StatePendingAcceptance {
			return nil, transitionError(id, v.status, StateRejected)
		}
		rec := v.rec
		rec.Status, rec.StatusMessage = StateRejected, rejectedMessage(accountID)
		rec.TerminalAt, rec.TerminatedBy = s.now(), accountID
		rec.Generation++
		if err := s.moveTerminal(ctx, &rec, v.rev); err != nil {
			if errors.Is(err, errConflict) {
				continue
			}
			return nil, internalError(ctx, "RejectVpcPeeringConnection", err)
		}
		slog.InfoContext(ctx, "RejectVpcPeeringConnection completed", "vpcPeeringConnectionId", id, "accountID", accountID)
		return &ec2.RejectVpcPeeringConnectionOutput{Return: aws.Bool(true)}, nil
	}
	return nil, internalError(ctx, "RejectVpcPeeringConnection", fmt.Errorf("reject %s: %w", id, errConflict))
}

// DeleteVpcPeeringConnection deletes a connection. A pending request moves
// straight to the terminal bucket; a connection with a link records the
// cleanup obligation and returns, and the reconciler finishes it.
func (s *ServiceImpl) DeleteVpcPeeringConnection(ctx context.Context, input *ec2.DeleteVpcPeeringConnectionInput, accountID string) (*ec2.DeleteVpcPeeringConnectionOutput, error) {
	id := aws.StringValue(input.VpcPeeringConnectionId)
	for range moveAttempts {
		v, err := s.visible(ctx, id, accountID)
		if err != nil {
			return nil, internalError(ctx, "DeleteVpcPeeringConnection", err)
		}
		if v == nil {
			return nil, notFoundError(id)
		}
		switch {
		case v.status == StateDeleted:
			return &ec2.DeleteVpcPeeringConnectionOutput{Return: aws.Bool(true)}, nil
		case v.status == StatePendingAcceptance && accountID == v.rec.RequesterAccountID,
			v.isLinkedAlive():
		default:
			return nil, transitionError(id, v.status, StateDeleting)
		}

		rec := v.rec
		rec.Status, rec.StatusMessage = StateDeleted, deletedMessage(accountID)
		rec.TerminalAt, rec.TerminatedBy = s.now(), accountID
		rec.Generation++
		if v.status == StatePendingAcceptance {
			err = s.moveTerminal(ctx, &rec, v.rev)
		} else {
			rec.CleanupPending = true
			if _, err = writeJSON(ctx, s.live, recordKey(id), &rec, v.rev); err == nil {
				err = s.releaseClaim(ctx, pairKey(rec.RequesterVpcID, rec.AccepterVpcID), id)
			}
		}
		if err != nil {
			if errors.Is(err, errConflict) {
				continue
			}
			return nil, internalError(ctx, "DeleteVpcPeeringConnection", err)
		}
		slog.InfoContext(ctx, "DeleteVpcPeeringConnection recorded", "vpcPeeringConnectionId", id,
			"cleanupPending", rec.CleanupPending, "accountID", accountID)
		return &ec2.DeleteVpcPeeringConnectionOutput{Return: aws.Bool(true)}, nil
	}
	return nil, internalError(ctx, "DeleteVpcPeeringConnection", fmt.Errorf("delete %s: %w", id, errConflict))
}

// ModifyVpcPeeringConnectionOptions sets an active connection's options. The
// DNS option is stored and described; it changes no resolution.
func (s *ServiceImpl) ModifyVpcPeeringConnectionOptions(ctx context.Context, input *ec2.ModifyVpcPeeringConnectionOptionsInput, accountID string) (*ec2.ModifyVpcPeeringConnectionOptionsOutput, error) {
	id := aws.StringValue(input.VpcPeeringConnectionId)
	for range moveAttempts {
		v, err := s.visible(ctx, id, accountID)
		if err != nil {
			return nil, internalError(ctx, "ModifyVpcPeeringConnectionOptions", err)
		}
		if v == nil {
			return nil, modifyNotFoundError(id)
		}
		if v.status != StateActive {
			return nil, notActiveError(id)
		}
		rec := v.rec
		sides := []struct {
			req   *ec2.PeeringConnectionOptionsRequest
			owner string
			vpcID string
			cidr  string
			opts  *PeeringOptions
		}{
			{input.RequesterPeeringConnectionOptions, rec.RequesterAccountID, rec.RequesterVpcID, rec.RequesterCIDR, &rec.RequesterOptions},
			{input.AccepterPeeringConnectionOptions, rec.AccepterAccountID, rec.AccepterVpcID, rec.AccepterCIDR, &rec.AccepterOptions},
		}
		for _, side := range sides {
			if side.req == nil {
				continue
			}
			if side.owner != accountID {
				return nil, awserrors.Errorf(awserrors.ErrorOperationNotPermitted,
					"Peering options for %s can be modified only by the owner of %s.", id, side.vpcID)
			}
			if aws.BoolValue(side.req.AllowDnsResolutionFromRemoteVpc) {
				vpc, err := s.vpcRecord(ctx, side.owner, side.vpcID)
				if err != nil {
					return nil, internalError(ctx, "ModifyVpcPeeringConnectionOptions", err)
				}
				if vpc == nil || !vpc.EnableDnsHostnames || !vpc.EnableDnsSupport {
					return nil, awserrors.Errorf(awserrors.ErrorInvalidVpcPeeringConnectionStateDnsHostnamesDisabled,
						"Public Hostnames are disabled for: %s", side.vpcID)
				}
			}
			if aws.BoolValue(side.req.AllowEgressFromLocalVpcToRemoteClassicLink) && overlapsClassicIPSpace(side.cidr) {
				return nil, awserrors.Errorf(awserrors.ErrorCidrConflict,
					"The CIDR range of %s overlaps with the Classic IP space", side.vpcID)
			}
			applyOptions(side.opts, side.req)
		}
		rec.Generation++
		if _, err := writeJSON(ctx, s.live, recordKey(id), &rec, v.rev); err != nil {
			if errors.Is(err, errConflict) {
				continue
			}
			return nil, internalError(ctx, "ModifyVpcPeeringConnectionOptions", err)
		}
		return &ec2.ModifyVpcPeeringConnectionOptionsOutput{
			RequesterPeeringConnectionOptions: optionsDescription(rec.RequesterOptions),
			AccepterPeeringConnectionOptions:  optionsDescription(rec.AccepterOptions),
		}, nil
	}
	return nil, internalError(ctx, "ModifyVpcPeeringConnectionOptions", fmt.Errorf("modify %s: %w", id, errConflict))
}

func applyOptions(o *PeeringOptions, req *ec2.PeeringConnectionOptionsRequest) {
	if req.AllowDnsResolutionFromRemoteVpc != nil {
		o.AllowDnsResolutionFromRemoteVpc = *req.AllowDnsResolutionFromRemoteVpc
	}
	if req.AllowEgressFromLocalClassicLinkToRemoteVpc != nil {
		o.AllowEgressFromLocalClassicLinkToRemoteVpc = *req.AllowEgressFromLocalClassicLinkToRemoteVpc
	}
	if req.AllowEgressFromLocalVpcToRemoteClassicLink != nil {
		o.AllowEgressFromLocalVpcToRemoteClassicLink = *req.AllowEgressFromLocalVpcToRemoteClassicLink
	}
}
