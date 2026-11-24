package accountlifecycle

//test:in-package — the reapers, the states they skip and the final reread
// are unexported, and which states they skip is the substance of them.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	ec2tgw "github.com/mulgadc/spinifex/spinifex/domains/ec2/tgw"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTransitGateways serves attachments and gateways in pages of one, so a
// lister that stops at the first page misses records.
type fakeTransitGateways struct {
	ec2tgw.TransitGatewayService

	mu          sync.Mutex
	attachments []*ec2.TransitGatewayVpcAttachment
	gateways    []*ec2.TransitGateway
	describeErr error
	deleteErr   error
	// hidden keeps the gateways out of Describe until it is cleared.
	hidden bool

	deletedAttachments []string
	deletedGateways    []string
}

func page[T any](items []*T, token *string) ([]*T, *string) {
	i := 0
	if t := aws.StringValue(token); t != "" {
		for i < len(items) && "p"+string(rune('0'+i)) != t {
			i++
		}
	}
	if i >= len(items) {
		return nil, nil
	}
	if i+1 < len(items) {
		return items[i : i+1], aws.String("p" + string(rune('0'+i+1)))
	}
	return items[i : i+1], nil
}

func (f *fakeTransitGateways) DescribeTransitGatewayVpcAttachments(_ context.Context, in *ec2.DescribeTransitGatewayVpcAttachmentsInput, _ string) (*ec2.DescribeTransitGatewayVpcAttachmentsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	items, next := page(f.attachments, in.NextToken)
	return &ec2.DescribeTransitGatewayVpcAttachmentsOutput{TransitGatewayVpcAttachments: items, NextToken: next}, nil
}

func (f *fakeTransitGateways) DescribeTransitGateways(_ context.Context, in *ec2.DescribeTransitGatewaysInput, _ string) (*ec2.DescribeTransitGatewaysOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	if f.hidden {
		return &ec2.DescribeTransitGatewaysOutput{}, nil
	}
	items, next := page(f.gateways, in.NextToken)
	return &ec2.DescribeTransitGatewaysOutput{TransitGateways: items, NextToken: next}, nil
}

func (f *fakeTransitGateways) DeleteTransitGatewayVpcAttachment(_ context.Context, in *ec2.DeleteTransitGatewayVpcAttachmentInput, _ string) (*ec2.DeleteTransitGatewayVpcAttachmentOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	id := aws.StringValue(in.TransitGatewayAttachmentId)
	f.deletedAttachments = append(f.deletedAttachments, id)
	for _, a := range f.attachments {
		if aws.StringValue(a.TransitGatewayAttachmentId) == id {
			a.State = aws.String(ec2tgw.StateDeleted)
		}
	}
	return &ec2.DeleteTransitGatewayVpcAttachmentOutput{}, nil
}

// DeleteTransitGateway refuses while any attachment of the gateway is live, as
// the service does.
func (f *fakeTransitGateways) DeleteTransitGateway(_ context.Context, in *ec2.DeleteTransitGatewayInput, _ string) (*ec2.DeleteTransitGatewayOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	id := aws.StringValue(in.TransitGatewayId)
	for _, a := range f.attachments {
		state := aws.StringValue(a.State)
		if aws.StringValue(a.TransitGatewayId) == id && state != ec2tgw.StateDeleted && state != ec2tgw.StateFailed {
			return nil, awserrors.Errorf(awserrors.ErrorIncorrectState, "%s has non-deleted VPC Attachments: %s.", id, aws.StringValue(a.TransitGatewayAttachmentId))
		}
	}
	f.deletedGateways = append(f.deletedGateways, id)
	for _, g := range f.gateways {
		if aws.StringValue(g.TransitGatewayId) == id {
			g.State = aws.String(ec2tgw.StateDeleted)
		}
	}
	return &ec2.DeleteTransitGatewayOutput{}, nil
}

func attachment(id, tgwID, state string) *ec2.TransitGatewayVpcAttachment {
	return &ec2.TransitGatewayVpcAttachment{TransitGatewayAttachmentId: aws.String(id), TransitGatewayId: aws.String(tgwID), State: aws.String(state)}
}

func gateway(id, state string) *ec2.TransitGateway {
	return &ec2.TransitGateway{TransitGatewayId: aws.String(id), State: aws.String(state)}
}

func ids(found []Resource) []string {
	out := make([]string, len(found))
	for i, r := range found {
		out[i] = r.ID
	}
	return out
}

func TestTransitGatewayReapersRunAttachmentsFirstInTheNetworkStage(t *testing.T) {
	t.Parallel()
	reapers := TransitGatewayReapers(nil)
	require.Len(t, reapers, 2)
	assert.IsType(t, &tgwAttachmentReaper{}, reapers[0])
	assert.IsType(t, &transitGatewayReaper{}, reapers[1])
	for _, r := range reapers {
		assert.Equal(t, StageNetwork, r.Stage())
		_, reread := r.(finalRereader)
		assert.True(t, reread, "%s is reread before the account record is removed", r.Kind())
	}
}

// A deleting attachment still blocks its VPC and its gateway, so it lists
// until it is deleted; deleted and failed ones hold nothing.
func TestAttachmentReaperListsEveryPageAndKeepsDeleting(t *testing.T) {
	t.Parallel()
	svc := &fakeTransitGateways{attachments: []*ec2.TransitGatewayVpcAttachment{
		attachment("tgw-attach-1", "tgw-1", ec2tgw.StatePending),
		attachment("tgw-attach-2", "tgw-1", ec2tgw.StateDeleted),
		attachment("tgw-attach-3", "tgw-1", ec2tgw.StateAvailable),
		attachment("tgw-attach-4", "tgw-1", ec2tgw.StateFailed),
		attachment("tgw-attach-5", "tgw-1", ec2tgw.StateDeleting),
	}}
	found, err := (&tgwAttachmentReaper{svc: svc}).List(t.Context(), testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"tgw-attach-1", "tgw-attach-3", "tgw-attach-5"}, ids(found))
	assert.Equal(t, ec2tgw.StateDeleting, found[2].Detail)
}

func TestTransitGatewayReaperListsEveryPageAndKeepsDeleting(t *testing.T) {
	t.Parallel()
	svc := &fakeTransitGateways{gateways: []*ec2.TransitGateway{
		gateway("tgw-1", ec2tgw.StateAvailable),
		gateway("tgw-2", ec2tgw.StateDeleted),
		gateway("tgw-3", ec2tgw.StateDeleting),
		gateway("tgw-4", ec2tgw.StatePending),
	}}
	found, err := (&transitGatewayReaper{svc: svc}).List(t.Context(), testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"tgw-1", "tgw-3", "tgw-4"}, ids(found))
}

// Below the feature level the actions answer UnsupportedOperation and no
// record can exist; any other failure must not read as an empty account.
func TestTransitGatewayReapersListErrors(t *testing.T) {
	t.Parallel()
	notOfferedErr := awserrors.Errorf(awserrors.ErrorUnsupportedOperation, "Transit Gateways are not available.")
	for _, r := range TransitGatewayReapers(nil) {
		svc := &fakeTransitGateways{describeErr: notOfferedErr}
		setService(r, svc)
		found, err := r.List(t.Context(), testAccountID)
		require.NoError(t, err, r.Kind())
		assert.Empty(t, found, r.Kind())

		svc.describeErr = errors.New(awserrors.ErrorServerInternal)
		_, err = r.List(t.Context(), testAccountID)
		require.Error(t, err, r.Kind())
	}
}

func setService(r Reaper, svc ec2tgw.TransitGatewayService) {
	switch r := r.(type) {
	case *tgwAttachmentReaper:
		r.svc = svc
	case *transitGatewayReaper:
		r.svc = svc
	}
}

// Teardown re-runs after a crash, so a record already gone is done, not failed.
func TestTransitGatewayReapersDeleteIgnoresNotFound(t *testing.T) {
	t.Parallel()
	attSvc := &fakeTransitGateways{deleteErr: awserrors.Errorf(awserrors.ErrorInvalidTransitGatewayAttachmentIDNotFound, "gone")}
	require.NoError(t, (&tgwAttachmentReaper{svc: attSvc}).Delete(t.Context(), testAccountID, Resource{ID: "tgw-attach-1"}, false))
	tgwSvc := &fakeTransitGateways{deleteErr: awserrors.Errorf(awserrors.ErrorInvalidTransitGatewayIDNotFound, "gone")}
	require.NoError(t, (&transitGatewayReaper{svc: tgwSvc}).Delete(t.Context(), testAccountID, Resource{ID: "tgw-1"}, false))

	attSvc.deleteErr = errors.New(awserrors.ErrorServerInternal)
	require.Error(t, (&tgwAttachmentReaper{svc: attSvc}).Delete(t.Context(), testAccountID, Resource{ID: "tgw-attach-1"}, false))
}

func TestTransitGatewayDeleteRefusedWhileAnAttachmentIsLive(t *testing.T) {
	t.Parallel()
	svc := &fakeTransitGateways{
		attachments: []*ec2.TransitGatewayVpcAttachment{attachment("tgw-attach-1", "tgw-1", ec2tgw.StateAvailable)},
		gateways:    []*ec2.TransitGateway{gateway("tgw-1", ec2tgw.StateAvailable)},
	}
	err := (&transitGatewayReaper{svc: svc}).Delete(t.Context(), testAccountID, Resource{ID: "tgw-1"}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIncorrectState)
	assert.Empty(t, svc.deletedGateways)
}

// The network stage retries the refused gateway delete until the attachment
// reaper has removed its attachment, then removes the gateway.
func TestTeardownRemovesAttachmentsThenTheirGateway(t *testing.T) {
	t.Parallel()
	svc := &fakeTransitGateways{
		attachments: []*ec2.TransitGatewayVpcAttachment{attachment("tgw-attach-1", "tgw-1", ec2tgw.StateAvailable)},
		gateways:    []*ec2.TransitGateway{gateway("tgw-1", ec2tgw.StateAvailable)},
	}
	// The gateway reaper is registered first, so its first delete is refused.
	accounts := newFakeAccounts()
	engine := quietEngine(accounts, &transitGatewayReaper{svc: svc}, &tgwAttachmentReaper{svc: svc})

	_, err := engine.Teardown(t.Context(), request())
	require.NoError(t, err)
	assert.Equal(t, []string{"tgw-attach-1"}, svc.deletedAttachments)
	assert.Equal(t, []string{"tgw-1"}, svc.deletedGateways)
	assert.True(t, accounts.deleted)
}

func networkStage(t *testing.T, result *Result) StageResult {
	t.Helper()
	for _, s := range result.Stages {
		if s.Stage == StageNetwork {
			return s
		}
	}
	t.Fatal("no network stage in the result")
	return StageResult{}
}

// A gateway create admitted before the quiesce can commit after the network
// stage drained; the reread finds it and the account record is kept.
func TestFinalRereadKeepsTheAccountForAGatewayCreatedAfterTheStage(t *testing.T) {
	t.Parallel()
	svc := &fakeTransitGateways{hidden: true, gateways: []*ec2.TransitGateway{gateway("tgw-late", ec2tgw.StatePending)}}
	accounts := newFakeAccounts()
	engine := quietEngine(accounts, &tgwAttachmentReaper{svc: svc}, &transitGatewayReaper{svc: svc})
	engine.OnStage = func(_ context.Context, stage StageResult) {
		if stage.Stage == StageNetwork {
			svc.mu.Lock()
			svc.hidden = false
			svc.mu.Unlock()
		}
	}

	result, err := engine.Teardown(t.Context(), request())
	require.ErrorIs(t, err, ErrResourcesStuck)
	assert.False(t, accounts.deleted, "the account record keeps the late gateway's owner")
	assert.False(t, result.AccountDeleted)
	stuck := networkStage(t, result).Stuck
	require.Len(t, stuck, 1)
	assert.Equal(t, "transit-gateway", stuck[0].Resource.Kind)
	assert.Equal(t, "tgw-late", stuck[0].Resource.ID)
	assert.Empty(t, svc.deletedGateways, "the reread names the gateway and deletes nothing")
}

func TestFinalRereadFailureIsNamedNotTakenAsEmpty(t *testing.T) {
	t.Parallel()
	svc := &fakeTransitGateways{}
	accounts := newFakeAccounts()
	engine := quietEngine(accounts, &transitGatewayReaper{svc: svc})
	engine.OnStage = func(_ context.Context, stage StageResult) {
		if stage.Stage == StageNetwork {
			svc.mu.Lock()
			svc.describeErr = errors.New(awserrors.ErrorServerInternal)
			svc.mu.Unlock()
		}
	}

	result, err := engine.Teardown(t.Context(), request())
	require.ErrorIs(t, err, ErrResourcesStuck)
	assert.False(t, accounts.deleted)
	stuck := networkStage(t, result).Stuck
	require.Len(t, stuck, 1)
	assert.Equal(t, Resource{Kind: "inventory", ID: "transit-gateway"}, stuck[0].Resource)
	assert.Equal(t, operatorv1.AccountingReasonRereadIncomplete, stuck[0].Reason)
}

// Only the kinds that opt in are listed again: a reaper whose listing would
// now fail is not read after its stage drained.
func TestFinalRereadSkipsReapersThatDoNotOptIn(t *testing.T) {
	t.Parallel()
	plain := &fakeReaper{kind: "vpc", stage: StageNetwork}
	accounts := newFakeAccounts()
	engine := quietEngine(accounts, plain)
	engine.OnStage = func(_ context.Context, stage StageResult) {
		if stage.Stage == StageNetwork {
			plain.listErr = errors.New("listed after its stage")
		}
	}

	_, err := engine.Teardown(t.Context(), request())
	require.NoError(t, err)
	assert.True(t, accounts.deleted)
}
