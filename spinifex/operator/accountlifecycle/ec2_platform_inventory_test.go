package accountlifecycle

//test:in-package — the consumer interface, its adapter and the engine field
// that binds it are unexported.

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountinventory"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveInventory answers the inventory subject with reply's bytes, or not at all when nil.
func serveInventory(t *testing.T, nc *nats.Conn, reply func() []byte) {
	t.Helper()
	sub, err := nc.QueueSubscribe(ec2v1.PlatformInventorySubject, "spinifex-workers", func(msg *nats.Msg) {
		if data := reply(); data != nil {
			_ = msg.Respond(data)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())
}

func assertAllIncomplete(t *testing.T, reply ec2v1.PlatformInventoryReply, reason string) {
	t.Helper()
	assert.Equal(t, testAccountID, reply.AccountID)
	require.Len(t, reply.Kinds, 4)
	for _, report := range reply.Kinds {
		assert.Equal(t, ec2v1.PlatformStatusScanIncomplete, report.Status, report.Kind)
		assert.Equal(t, []string{reason}, report.IncompleteReasons, report.Kind)
	}
	assert.Empty(t, reply.Items)
	assert.Empty(t, reply.Counts)
}

// The adapter sends the account and correlation reference in the body and
// returns EC2's report of the owner stores, residue counts included.
func TestEC2PlatformInventoryClientReturnsEC2sReport(t *testing.T) {
	o := newPlatformOwners(t)
	ctx := testCtx(t)
	inv := accountinventory.New(o.keys, o.groups, o.templates, o.tags)
	var seen ec2v1.PlatformInventoryRequest
	sub, err := o.nc.QueueSubscribe(ec2v1.PlatformInventorySubject, "spinifex-workers", func(msg *nats.Msg) {
		natsmsg.ServeNATSRequestCtx(msg, func(ctx context.Context, req *ec2v1.PlatformInventoryRequest) (*ec2v1.PlatformInventoryReply, error) {
			seen = *req
			return inv.PlatformInventory(ctx, req)
		})
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	_, err = o.keys.ImportKeyPair(ctx, &ec2.ImportKeyPairInput{
		KeyName: aws.String("deploy"), PublicKeyMaterial: []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"),
	}, testAccountID)
	require.NoError(t, err)
	_, err = o.keys.DeleteKeyPair(ctx, &ec2.DeleteKeyPairInput{KeyName: aws.String("deploy")}, testAccountID)
	require.NoError(t, err)
	// A public key without metadata, as an older release's failed delete left.
	_, err = o.store.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(platformBucket), Key: aws.String("keys/" + testAccountID + "/deploy"), Body: strings.NewReader("ssh-ed25519 AAAA"),
	})
	require.NoError(t, err)

	reply := newEC2PlatformInventoryClient(o.nc).PlatformInventory(ctx, testAccountID, "req-1")

	assert.Equal(t, ec2v1.PlatformInventoryRequest{AccountID: testAccountID, CorrelationRef: "req-1"}, seen)
	assert.Equal(t, "req-1", reply.CorrelationRef)
	assert.Equal(t, []ec2v1.PlatformRecordCount{{
		Kind: ec2v1.PlatformKindKeyPair, Disposition: ec2v1.PlatformDispositionResidue,
		Class: ec2v1.PlatformClassPublicKeyWithoutMetadata, Count: 1,
	}}, reply.Counts)
	statuses := map[string]string{}
	for _, report := range reply.Kinds {
		statuses[report.Kind] = report.Status
	}
	assert.Equal(t, map[string]string{
		ec2v1.PlatformKindKeyPair:        ec2v1.PlatformStatusRecordsFound,
		ec2v1.PlatformKindPlacementGroup: ec2v1.PlatformStatusObservedClean,
		ec2v1.PlatformKindLaunchTemplate: ec2v1.PlatformStatusObservedClean,
		ec2v1.PlatformKindTagIndex:       ec2v1.PlatformStatusObservedClean,
	}, statuses)
}

// No daemon serving the subject, which is also what a daemon predating it
// looks like, is scan-incomplete for every kind, never clean.
func TestEC2PlatformInventoryClientReportsNoResponderAsIncomplete(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	reply := newEC2PlatformInventoryClient(nc).PlatformInventory(testCtx(t), testAccountID, "req-1")
	assertAllIncomplete(t, reply, ec2v1.PlatformReasonNoResponder)
	assert.Equal(t, "req-1", reply.CorrelationRef)
}

func TestEC2PlatformInventoryClientReportsTransportFailuresAsIncomplete(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		_, nc, _ := testutil.StartTestJetStream(t)
		serveInventory(t, nc, func() []byte { return nil })
		client := newEC2PlatformInventoryClient(nc)
		client.timeout = 50 * time.Millisecond
		assertAllIncomplete(t, client.PlatformInventory(testCtx(t), testAccountID, ""), ec2v1.PlatformReasonTimeout)
	})
	t.Run("error reply", func(t *testing.T) {
		_, nc, _ := testutil.StartTestJetStream(t)
		serveInventory(t, nc, func() []byte {
			return awserrors.GenerateErrorPayload(awserrors.ErrorServerInternal)
		})
		assertAllIncomplete(t, newEC2PlatformInventoryClient(nc).PlatformInventory(testCtx(t), testAccountID, ""), ec2v1.PlatformReasonErrorReply)
	})
	t.Run("undecodable reply", func(t *testing.T) {
		_, nc, _ := testutil.StartTestJetStream(t)
		serveInventory(t, nc, func() []byte { return []byte(`{"kinds":"nope"}`) })
		assertAllIncomplete(t, newEC2PlatformInventoryClient(nc).PlatformInventory(testCtx(t), testAccountID, ""), ec2v1.PlatformReasonTransportFailed)
	})
	t.Run("closed connection", func(t *testing.T) {
		_, nc, _ := testutil.StartTestJetStream(t)
		nc.Close()
		assertAllIncomplete(t, newEC2PlatformInventoryClient(nc).PlatformInventory(testCtx(t), testAccountID, ""), ec2v1.PlatformReasonTransportFailed)
	})
}

// A reply for another account is discarded; a kind the reply omits or
// repeats, or a status this consumer does not know, is scan-incomplete.
func TestEC2PlatformInventoryClientDistrustsAMalformedReply(t *testing.T) {
	clean := ec2v1.PlatformKindReport{Kind: ec2v1.PlatformKindKeyPair, Status: ec2v1.PlatformStatusObservedClean}

	_, nc, _ := testutil.StartTestJetStream(t)
	serveInventory(t, nc, func() []byte {
		data, _ := json.Marshal(ec2v1.PlatformInventoryReply{AccountID: "999999999999", Kinds: []ec2v1.PlatformKindReport{clean}})
		return data
	})
	assertAllIncomplete(t, newEC2PlatformInventoryClient(nc).PlatformInventory(testCtx(t), testAccountID, ""), ec2v1.PlatformReasonInvalidReply)

	reply := normalisePlatformInventory(ec2v1.PlatformInventoryReply{AccountID: testAccountID, Kinds: []ec2v1.PlatformKindReport{
		clean,
		clean,
		{Kind: ec2v1.PlatformKindPlacementGroup, Status: "drained"},
		{Kind: ec2v1.PlatformKindTagIndex, Status: ec2v1.PlatformStatusRecordsFound},
	}}, testAccountID, "")
	assert.Equal(t, []ec2v1.PlatformKindReport{
		{Kind: ec2v1.PlatformKindKeyPair, Status: ec2v1.PlatformStatusScanIncomplete, IncompleteReasons: []string{ec2v1.PlatformReasonInvalidReply}},
		{Kind: ec2v1.PlatformKindPlacementGroup, Status: ec2v1.PlatformStatusScanIncomplete, IncompleteReasons: []string{ec2v1.PlatformReasonInvalidReply}},
		{Kind: ec2v1.PlatformKindLaunchTemplate, Status: ec2v1.PlatformStatusScanIncomplete, IncompleteReasons: []string{ec2v1.PlatformReasonKindNotReported}},
		{Kind: ec2v1.PlatformKindTagIndex, Status: ec2v1.PlatformStatusRecordsFound},
	}, reply.Kinds)
}

// The cluster wiring binds the NATS adapter, and only the explicit dry run
// consults it: precheck, the inventory the CLI prints before a real teardown,
// a Teardown request marked dry run and a full teardown all leave it uncalled.
func TestOnlyTheExplicitDryRunCallsTheEC2PlatformInventory(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)
	svc, err := iamdomain.NewIAMServiceImpl(t.Context(), nc, masterKey)
	require.NoError(t, err)
	cluster, err := NewClusterEngine(t.Context(), nc, 1, []string{"node-1"}, svc, newFakeBuckets())
	require.NoError(t, err)
	assert.IsType(t, &ec2PlatformInventoryClient{}, cluster.ec2Inventory)

	inventory := &fakeInventory{reply: ec2v1.IncompletePlatformInventory(testAccountID, "req-1", ec2v1.PlatformReasonNoResponder)}
	reaper := &fakeReaper{kind: "key-pair", stage: StagePlatform, resources: []Resource{{Kind: "key-pair", ID: "deploy"}}}
	engine := quietEngine(newFakeAccounts(), reaper)
	engine.ec2Inventory = inventory

	require.NoError(t, engine.Precheck(testAccountID, testAccountName))
	_, err = engine.Inventory(testCtx(t), testAccountID)
	require.NoError(t, err)
	dryRun := request()
	dryRun.DryRun = true
	_, err = engine.Teardown(testCtx(t), dryRun)
	require.NoError(t, err)
	assert.Empty(t, inventory.calls())

	_, err = engine.DryRun(testCtx(t), testAccountID, "req-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"req-1"}, inventory.calls())

	result, err := engine.Teardown(testCtx(t), request())
	require.NoError(t, err)
	assert.True(t, result.AccountDeleted)
	assert.Equal(t, []string{"req-1"}, inventory.calls(), "a real teardown never consults the inventory")
}

// The shadow matches legacy listings to inventory kinds by the reapers' kind
// names, so the registered platform reapers must use the contract's names.
func TestPlatformReaperKindsAreTheInventoryKinds(t *testing.T) {
	var kinds []string
	for _, reaper := range EC2Reapers(nil, 1, nil) {
		if reaper.Stage() == StagePlatform {
			kinds = append(kinds, reaper.Kind())
		}
	}
	for _, kind := range ec2ShadowKinds {
		assert.Contains(t, kinds, kind.kind)
	}
}

// A caller deadline shorter than the adapter's own timeout ends the request
// at that deadline, and a context already done sends nothing.
func TestEC2PlatformInventoryClientHonoursTheCallersDeadline(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	var requests atomic.Int32
	serveInventory(t, nc, func() []byte { requests.Add(1); return nil })
	client := newEC2PlatformInventoryClient(nc)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	assertAllIncomplete(t, client.PlatformInventory(ctx, testAccountID, ""), ec2v1.PlatformReasonTimeout)
	assert.Less(t, time.Since(started), 5*time.Second)
	assert.Equal(t, int32(1), requests.Load())

	done, cancelDone := context.WithCancel(t.Context())
	cancelDone()
	assertAllIncomplete(t, client.PlatformInventory(done, testAccountID, ""), ec2v1.PlatformReasonTimeout)
	assert.Equal(t, int32(1), requests.Load())
}
