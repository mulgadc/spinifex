package volume

//test:in-package — the obligation reaper and its policy are unexported, and
// seeding a volume's metadata document is how an attachment is set up.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	ebsobligation "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/obligation"
	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeNodeLocator struct {
	nodes map[string]string
	err   error
}

var _ InstanceNodeLocator = (*fakeNodeLocator)(nil)

func (f *fakeNodeLocator) InstanceLastNode(instanceID string) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	node, found := f.nodes[instanceID]
	return node, found, nil
}

// wireObligations gives svc an obligation bucket on a JetStream of its own and
// returns the connection under it, so a test can cut it.
func wireObligations(t *testing.T, svc *VolumeServiceImpl, locator InstanceNodeLocator, members ...string) *nats.Conn {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	store := ebsobligation.NewStore(js)
	require.NoError(t, store.Open(t.Context()))
	svc.SetUnpublishObligations(store, locator, func() []string { return members })
	return nc
}

type unpublishCall struct {
	volumeID, accountID, handle string
	noSeal                      bool
}

// fakeUnpublisher stands in for a node's mounter: busy volumes refuse the
// hold, publishedAt is the last publish this process made of each volume.
type fakeUnpublisher struct {
	mu          sync.Mutex
	busy        map[string]bool
	publishedAt map[string]time.Time
	err         error
	calls       []unpublishCall
	held        int
}

var _ ObligationUnpublisher = (*fakeUnpublisher)(nil)

func (f *fakeUnpublisher) HoldVolume(volumeID string) (VolumeHold, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy[volumeID] {
		return nil, false
	}
	f.held++
	return &fakeHold{f: f, volumeID: volumeID}, true
}

func (f *fakeUnpublisher) unpublishes() []unpublishCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]unpublishCall(nil), f.calls...)
}

type fakeHold struct {
	f        *fakeUnpublisher
	volumeID string
}

var _ VolumeHold = (*fakeHold)(nil)

func (h *fakeHold) PublishedSince(t time.Time) bool {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	at, ok := h.f.publishedAt[h.volumeID]
	return ok && at.After(t)
}

func (h *fakeHold) Unpublish(_ context.Context, accountID, handle string, noSeal bool) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.f.calls = append(h.f.calls, unpublishCall{volumeID: h.volumeID, accountID: accountID, handle: handle, noSeal: noSeal})
	return h.f.err
}

func (h *fakeHold) Release() {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.f.held--
}

// putAttachment writes volumeID's document with an explicit attachment, so a
// test controls AttachedAt rather than racing the clock.
func putAttachment(t *testing.T, svc *VolumeServiceImpl, volumeID, state, instanceID string, attachedAt time.Time) ebsmetadata.Volume {
	t.Helper()
	meta := ebsmetadata.Volume{
		VolumeID: volumeID, TenantID: testVolAccountID, CapacityGiB: 1,
		State: state, AvailabilityZone: "ap-southeast-2a", VolumeType: "gp3",
		AttachedInstance: instanceID, AttachedAt: attachedAt, ProviderHandle: "handle-" + volumeID,
	}
	if instanceID != "" {
		meta.DeviceName = "/dev/sdf"
	}
	require.NoError(t, svc.metadata.PutVolume(context.Background(), meta))
	return meta
}

func readVolume(t *testing.T, svc *VolumeServiceImpl, volumeID string) ebsmetadata.Volume {
	t.Helper()
	meta, err := svc.metadata.GetVolume(context.Background(), testVolAccountID, volumeID)
	require.NoError(t, err)
	return meta
}

func openObligations(t *testing.T, svc *VolumeServiceImpl) []ebsobligation.UnpublishObligation {
	t.Helper()
	open, err := svc.obligations.List(context.Background())
	require.NoError(t, err)
	return open
}

func forceDetach(t *testing.T, svc *VolumeServiceImpl, volumeID string) (*ec2.VolumeAttachment, error) {
	t.Helper()
	return svc.ForceDetachVolume(context.Background(), &ec2.DetachVolumeInput{
		VolumeId: aws.String(volumeID), Force: aws.Bool(true),
	}, testVolAccountID)
}

// --- ForceDetachVolume records the obligation ---

func TestForceDetach_RecordsOneObligationNamingThePublish(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	wireObligations(t, svc, &fakeNodeLocator{nodes: map[string]string{"i-old": "node-a"}}, "node-a", "node-b")
	attachedAt := time.Unix(1_700_000_000, 0).UTC()
	putAttachment(t, svc, "vol-fd-once", "in-use", "i-old", attachedAt)

	_, err := forceDetach(t, svc, "vol-fd-once")
	require.NoError(t, err)

	open := openObligations(t, svc)
	require.Len(t, open, 1, "only the node that last ran the instance is named")
	o := open[0]
	assert.Equal(t, "vol-fd-once", o.VolumeID)
	assert.Equal(t, testVolAccountID, o.AccountID)
	assert.Equal(t, "handle-vol-fd-once", o.Handle)
	assert.Equal(t, "node-a", o.NodeID)
	assert.Equal(t, "i-old", o.InstanceID)
	assert.True(t, attachedAt.Equal(o.AttachedAt), "the obligation names the attachment it undoes")
	assert.Equal(t, ebsobligation.SchemaVersion, o.SchemaVersion)
}

// A caller retrying a force detach whose clear failed writes the same key, so
// the retry leaves one obligation, not two.
func TestForceDetach_RetryLeavesOneObligation(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	wireObligations(t, svc, &fakeNodeLocator{nodes: map[string]string{"i-old": "node-a"}}, "node-a")
	meta := putAttachment(t, svc, "vol-fd-retry", "in-use", "i-old", time.Unix(1_700_000_000, 0).UTC())

	require.NoError(t, svc.recordObligations(context.Background(), meta, []string{"node-a"}))
	_, err := forceDetach(t, svc, "vol-fd-retry")
	require.NoError(t, err)

	assert.Len(t, openObligations(t, svc), 1)
}

// With no record of the instance there is no node to name, so every member is
// named and each one settles its own.
func TestForceDetach_UnknownInstanceNamesEveryMember(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	wireObligations(t, svc, &fakeNodeLocator{}, "node-a", "node-b", "node-c")
	putAttachment(t, svc, "vol-fd-every", "in-use", "i-gone", time.Unix(1_700_000_000, 0).UTC())

	_, err := forceDetach(t, svc, "vol-fd-every")
	require.NoError(t, err)

	nodes := []string{}
	for _, o := range openObligations(t, svc) {
		nodes = append(nodes, o.NodeID)
	}
	assert.ElementsMatch(t, []string{"node-a", "node-b", "node-c"}, nodes)
}

// The attachment is the only record of where the export is. If the obligation
// cannot be written, clearing it would forget an export, so nothing is cleared.
func TestForceDetach_ObligationWriteFailureClearsNothing(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	nc := wireObligations(t, svc, &fakeNodeLocator{nodes: map[string]string{"i-old": "node-a"}}, "node-a")
	putAttachment(t, svc, "vol-fd-nowrite", "in-use", "i-old", time.Unix(1_700_000_000, 0).UTC())
	nc.Close()

	_, err := forceDetach(t, svc, "vol-fd-nowrite")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
	meta := readVolume(t, svc, "vol-fd-nowrite")
	assert.Equal(t, "in-use", meta.State)
	assert.Equal(t, "i-old", meta.AttachedInstance)
}

func TestForceDetach_UnnameableNodeClearsNothing(t *testing.T) {
	for name, locator := range map[string]InstanceNodeLocator{
		"locator fails":            &fakeNodeLocator{err: errors.New("instance bucket unreachable")},
		"no record and no members": &fakeNodeLocator{},
		"no locator configured":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			svc := newTestVolumeService("ap-southeast-2a")
			wireObligations(t, svc, locator)
			putAttachment(t, svc, "vol-fd-noname", "in-use", "i-old", time.Unix(1_700_000_000, 0).UTC())

			_, err := forceDetach(t, svc, "vol-fd-noname")

			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInternalError, err.Error())
			assert.Equal(t, "i-old", readVolume(t, svc, "vol-fd-noname").AttachedInstance)
			assert.Empty(t, openObligations(t, svc))
		})
	}
}

// providerCallCounter fails the test's expectations if the request path makes
// any provider call: an unpublish waits on a node that may never answer.
type providerCallCounter struct {
	ebsprovider.EBSProvider

	mu        sync.Mutex
	unpublish int
	publish   int
}

func (p *providerCallCounter) UnpublishVolume(ctx context.Context, req ebsprovider.UnpublishVolumeRequest) (*ebsprovider.UnpublishedVolume, error) {
	p.mu.Lock()
	p.unpublish++
	p.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (p *providerCallCounter) PublishVolume(ctx context.Context, req ebsprovider.PublishVolumeRequest) (*ebsprovider.PublishedVolume, error) {
	p.mu.Lock()
	p.publish++
	p.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestS8_ForceDetachRecordsTheObligationAndReturns pins that a force detach
// never waits on the node holding the export: it decides its errors, records
// the obligation and returns, and the unpublish is left to that node's reaper.
func TestS8_ForceDetachRecordsTheObligationAndReturns(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	provider := &providerCallCounter{EBSProvider: ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})}
	svc.SetEBSProvider(provider)
	wireObligations(t, svc, &fakeNodeLocator{nodes: map[string]string{"i-wedged": "node-unreachable"}}, "node-unreachable")
	putAttachment(t, svc, "vol-s8-forcedetach", "in-use", "i-wedged", time.Unix(1_700_000_000, 0).UTC())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	attachment, err := svc.ForceDetachVolume(ctx, &ec2.DetachVolumeInput{
		VolumeId: aws.String("vol-s8-forcedetach"), Force: aws.Bool(true),
	}, testVolAccountID)
	require.NoError(t, err)
	require.NoError(t, ctx.Err(), "the request returned on its own, not on the deadline")

	assert.Equal(t, "detaching", aws.StringValue(attachment.State))
	provider.mu.Lock()
	assert.Zero(t, provider.unpublish, "the request path makes no unpublish call")
	assert.Zero(t, provider.publish)
	provider.mu.Unlock()

	open := openObligations(t, svc)
	require.Len(t, open, 1, "the unpublish the request did not wait for is recorded")
	assert.Equal(t, "node-unreachable", open[0].NodeID)
	assert.Empty(t, readVolume(t, svc, "vol-s8-forcedetach").AttachedInstance)
}

// --- Terminate paths ---

func TestTerminate_OwedUnpublishRecordsOneObligationBeforeTheClear(t *testing.T) {
	for name, terminate := range map[string]func(*VolumeServiceImpl, string, ec2instance.OwedUnpublish) error{
		"detach": func(svc *VolumeServiceImpl, id string, owed ec2instance.OwedUnpublish) error {
			return svc.DetachVolumeOnTerminate(context.Background(), id, testVolAccountID, owed)
		},
		"delete": func(svc *VolumeServiceImpl, id string, owed ec2instance.OwedUnpublish) error {
			return svc.DeleteVolumeOnTerminate(context.Background(), id, testVolAccountID, owed)
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc := newTestVolumeService("ap-southeast-2a")
			svc.snapshotKV = setupTestVolumeKV(t)
			wireObligations(t, svc, &fakeNodeLocator{}, "node-a")
			seedTestProviderVolume(t, svc, "vol-term-owed", 1)
			putAttachment(t, svc, "vol-term-owed", "in-use", "i-term", time.Unix(1_700_000_000, 0).UTC())

			require.NoError(t, terminate(svc, "vol-term-owed", ec2instance.OwedUnpublish{NodeID: "node-a", InstanceID: "i-term"}))

			open := openObligations(t, svc)
			require.Len(t, open, 1)
			assert.Equal(t, "node-a", open[0].NodeID)
			assert.Equal(t, "i-term", open[0].InstanceID)
		})
	}
}

// A terminate whose unmount completed owes nothing, and an attachment that is
// already some other instance's is not this terminate's to undo.
func TestTerminate_NothingOwedRecordsNoObligation(t *testing.T) {
	for name, tc := range map[string]struct {
		attachedTo string
		owed       ec2instance.OwedUnpublish
	}{
		"unmount completed":   {attachedTo: "i-term", owed: ec2instance.OwedUnpublish{}},
		"attached to another": {attachedTo: "i-other", owed: ec2instance.OwedUnpublish{NodeID: "node-a", InstanceID: "i-term"}},
	} {
		t.Run(name, func(t *testing.T) {
			svc := newTestVolumeService("ap-southeast-2a")
			wireObligations(t, svc, &fakeNodeLocator{}, "node-a")
			putAttachment(t, svc, "vol-term-none", "in-use", tc.attachedTo, time.Unix(1_700_000_000, 0).UTC())

			require.NoError(t, svc.DetachVolumeOnTerminate(context.Background(), "vol-term-none", testVolAccountID, tc.owed))

			assert.Empty(t, openObligations(t, svc))
		})
	}
}

func TestTerminate_ObligationWriteFailureClearsNothing(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	nc := wireObligations(t, svc, &fakeNodeLocator{}, "node-a")
	putAttachment(t, svc, "vol-term-nowrite", "in-use", "i-term", time.Unix(1_700_000_000, 0).UTC())
	nc.Close()

	err := svc.DetachVolumeOnTerminate(context.Background(), "vol-term-nowrite", testVolAccountID,
		ec2instance.OwedUnpublish{NodeID: "node-a", InstanceID: "i-term"})

	require.Error(t, err)
	assert.Equal(t, "i-term", readVolume(t, svc, "vol-term-nowrite").AttachedInstance)
}

// --- The reaper ---

func recordObligation(t *testing.T, svc *VolumeServiceImpl, node, volumeID, instanceID string, attachedAt time.Time) ebsobligation.UnpublishObligation {
	t.Helper()
	o := ebsobligation.UnpublishObligation{
		VolumeID: volumeID, AccountID: testVolAccountID, Handle: "handle-" + volumeID,
		NodeID: node, InstanceID: instanceID, AttachedAt: attachedAt, RecordedAt: attachedAt.Add(time.Minute),
	}
	require.NoError(t, svc.obligations.Record(context.Background(), o))
	return o
}

func sweepObligations(t *testing.T, svc *VolumeServiceImpl, node string, unpublisher ObligationUnpublisher) int {
	t.Helper()
	closed, err := svc.NewUnpublishObligationReaper(node, unpublisher).Sweep(context.Background())
	require.NoError(t, err)
	return closed
}

// TestS7_ObligationReaperNeverUnpublishesACurrentAttachment pins that the
// reaper, which runs with no customer asking, never tears down an export a
// live attachment uses and never removes or rewrites the volume.
func TestS7_ObligationReaperNeverUnpublishesACurrentAttachment(t *testing.T) {
	cleared := time.Unix(1_700_000_000, 0).UTC()
	for name, tc := range map[string]struct {
		state       string
		attachedTo  string
		attachedAt  time.Time
		publishedAt time.Time // a publish this process made, zero if none
		stillOpen   bool
	}{
		// The volume moved to another instance, possibly on this very node.
		"attached to another instance": {state: "in-use", attachedTo: "i-new", attachedAt: cleared.Add(time.Hour)},
		// The same instance was attached again after the clear.
		"same instance attached again": {state: "in-use", attachedTo: "i-old", attachedAt: cleared.Add(time.Hour)},
		// The clear has not landed: the obligation was written first.
		"attachment not yet cleared": {state: "in-use", attachedTo: "i-old", attachedAt: cleared, stillOpen: true},
		// A hot attach published here and has not yet written in-use.
		"newer publish here, record not yet in-use": {state: "available", publishedAt: cleared.Add(time.Hour), stillOpen: true},
	} {
		t.Run(name, func(t *testing.T) {
			svc := newTestVolumeService("ap-southeast-2a")
			wireObligations(t, svc, &fakeNodeLocator{}, "node-a")
			before := putAttachment(t, svc, "vol-s7-current", tc.state, tc.attachedTo, tc.attachedAt)
			recordObligation(t, svc, "node-a", "vol-s7-current", "i-old", cleared)
			unpublisher := &fakeUnpublisher{publishedAt: map[string]time.Time{}}
			if !tc.publishedAt.IsZero() {
				unpublisher.publishedAt["vol-s7-current"] = tc.publishedAt
			}

			sweepObligations(t, svc, "node-a", unpublisher)

			assert.Empty(t, unpublisher.unpublishes(), "a current attachment's export must never be unpublished")
			assert.Zero(t, unpublisher.held, "every hold is released")
			after := readVolume(t, svc, "vol-s7-current")
			assert.Equal(t, before.State, after.State, "the reaper never rewrites the volume")
			assert.Equal(t, before.AttachedInstance, after.AttachedInstance)
			if tc.stillOpen {
				assert.Len(t, openObligations(t, svc), 1, "the obligation waits for its attachment to be gone")
			} else {
				assert.Empty(t, openObligations(t, svc), "a superseded obligation closes without a provider call")
			}
		})
	}
}

// TestS7_ObligationReaperSealsLiveDataAndDropsOnlyDeletingVolumes pins that
// the reaper keeps a surviving volume's writes and only drops them unsealed
// for a volume that is being deleted or is gone.
func TestS7_ObligationReaperSealsLiveDataAndDropsOnlyDeletingVolumes(t *testing.T) {
	cleared := time.Unix(1_700_000_000, 0).UTC()
	for name, tc := range map[string]struct {
		state  string // empty for no document
		noSeal bool
	}{
		"available keeps its writes": {state: "available", noSeal: false},
		"deleting is dropped":        {state: volumeStateDeleting, noSeal: true},
		"absent is dropped":          {state: "", noSeal: true},
	} {
		t.Run(name, func(t *testing.T) {
			svc := newTestVolumeService("ap-southeast-2a")
			wireObligations(t, svc, &fakeNodeLocator{}, "node-a")
			if tc.state != "" {
				putAttachment(t, svc, "vol-s7-settle", tc.state, "", time.Time{})
			}
			recordObligation(t, svc, "node-a", "vol-s7-settle", "i-old", cleared)
			unpublisher := &fakeUnpublisher{}

			assert.Equal(t, 1, sweepObligations(t, svc, "node-a", unpublisher))

			assert.Equal(t, []unpublishCall{{volumeID: "vol-s7-settle", accountID: testVolAccountID, handle: "handle-vol-s7-settle", noSeal: tc.noSeal}},
				unpublisher.unpublishes())
			assert.Empty(t, openObligations(t, svc))
			if tc.state != "" {
				assert.Equal(t, tc.state, readVolume(t, svc, "vol-s7-settle").State, "the reaper never rewrites the volume")
			}
		})
	}
}

// TestS7_ObligationReaperNeverTreatsAnUnreadableVolumeAsGone pins that a read
// failing for a reason other than absence leaves the obligation open: taking
// it for a deleted volume would drop live writes unsealed.
func TestS7_ObligationReaperNeverTreatsAnUnreadableVolumeAsGone(t *testing.T) {
	memory := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", memory)
	wireObligations(t, svc, &fakeNodeLocator{}, "node-a")
	putAttachment(t, svc, "vol-s7-unread", "available", "", time.Time{})
	recordObligation(t, svc, "node-a", "vol-s7-unread", "i-old", time.Unix(1_700_000_000, 0).UTC())
	failing := &getFailingStore{MemoryObjectStore: memory, failKey: metadataKey(t, "vol-s7-unread")}
	svc.metadata = ebsmetadata.NewStore(failing, "test-bucket")
	unpublisher := &fakeUnpublisher{}

	assert.Zero(t, sweepObligations(t, svc, "node-a", unpublisher))

	assert.Empty(t, unpublisher.unpublishes())
	open := openObligations(t, svc)
	require.Len(t, open, 1)
	assert.Equal(t, 1, open[0].Attempts, "the failure is counted where an operator can see it")
	assert.Contains(t, open[0].Error, "reconstruction failed")
}

// End to end through the service: a force detach followed by a new attachment
// of the same volume on the same node must leave that new export alone.
func TestS7_ForceDetachThenReattachOnTheSameNodeKeepsTheNewExport(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	wireObligations(t, svc, &fakeNodeLocator{nodes: map[string]string{"i-old": "node-a", "i-new": "node-a"}}, "node-a")
	putAttachment(t, svc, "vol-s7-reattach", "in-use", "i-old", time.Now().Add(-time.Hour).UTC())

	_, err := forceDetach(t, svc, "vol-s7-reattach")
	require.NoError(t, err)
	require.NoError(t, svc.UpdateVolumeState(testVolAccountID, "vol-s7-reattach", "in-use", "i-new", "/dev/sdg"))
	unpublisher := &fakeUnpublisher{}

	assert.Equal(t, 1, sweepObligations(t, svc, "node-a", unpublisher))

	assert.Empty(t, unpublisher.unpublishes(), "the new attachment's export must survive the old obligation")
	assert.Equal(t, "i-new", readVolume(t, svc, "vol-s7-reattach").AttachedInstance)
	assert.Empty(t, openObligations(t, svc))
}

func TestObligationReaper_SettlesOnlyItsOwnNode(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	wireObligations(t, svc, &fakeNodeLocator{}, "node-a", "node-b")
	putAttachment(t, svc, "vol-own", "available", "", time.Time{})
	recordObligation(t, svc, "node-b", "vol-own", "i-old", time.Unix(1_700_000_000, 0).UTC())
	unpublisher := &fakeUnpublisher{}

	assert.Zero(t, sweepObligations(t, svc, "node-a", unpublisher))

	assert.Empty(t, unpublisher.unpublishes(), "only the node holding the export can tear it down")
	assert.Len(t, openObligations(t, svc), 1)
}

// A publish or unpublish of the volume in flight on this node holds its lock;
// the sweep skips it rather than waiting behind a possibly wedged call.
func TestObligationReaper_SkipsABusyVolume(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	wireObligations(t, svc, &fakeNodeLocator{}, "node-a")
	putAttachment(t, svc, "vol-busy", "available", "", time.Time{})
	recordObligation(t, svc, "node-a", "vol-busy", "i-old", time.Unix(1_700_000_000, 0).UTC())
	unpublisher := &fakeUnpublisher{busy: map[string]bool{"vol-busy": true}}

	assert.Zero(t, sweepObligations(t, svc, "node-a", unpublisher))

	assert.Empty(t, unpublisher.unpublishes())
	open := openObligations(t, svc)
	require.Len(t, open, 1)
	assert.Zero(t, open[0].Attempts, "a busy volume is not a failure")
}

func TestObligationReaper_FailedUnpublishStaysOpenAndCounts(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	wireObligations(t, svc, &fakeNodeLocator{}, "node-a")
	putAttachment(t, svc, "vol-fail", "available", "", time.Time{})
	recordObligation(t, svc, "node-a", "vol-fail", "i-old", time.Unix(1_700_000_000, 0).UTC())
	unpublisher := &fakeUnpublisher{err: errors.New("seal refused")}

	assert.Zero(t, sweepObligations(t, svc, "node-a", unpublisher))
	assert.Zero(t, sweepObligations(t, svc, "node-a", unpublisher))

	open := openObligations(t, svc)
	require.Len(t, open, 1)
	assert.Equal(t, 2, open[0].Attempts)
	assert.Equal(t, "seal refused", open[0].Error)
	assert.Zero(t, unpublisher.held, "a failed unpublish still releases the hold")
}

// An obligation with no attachment time falls back to when it was recorded, so
// a publish here after the clear still counts as newer.
func TestObligationSince_FallsBackToRecordedAt(t *testing.T) {
	recorded := time.Unix(1_700_000_000, 0)
	assert.Equal(t, recorded, obligationSince(ebsobligation.UnpublishObligation{RecordedAt: recorded}))
	attached := recorded.Add(-time.Hour)
	assert.Equal(t, attached, obligationSince(ebsobligation.UnpublishObligation{AttachedAt: attached, RecordedAt: recorded}))
}

// --- The cluster-wide close ---

func TestObligationCloser_ClosesOnlyDepartedNodes(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	wireObligations(t, svc, &fakeNodeLocator{}, "node-a")
	at := time.Unix(1_700_000_000, 0).UTC()
	recordObligation(t, svc, "node-gone", "vol-c1", "i-1", at)
	recordObligation(t, svc, "node-gone", "vol-c2", "i-2", at)
	recordObligation(t, svc, "node-down", "vol-c3", "i-3", at)
	recordObligation(t, svc, "node-unknown", "vol-c4", "i-4", at)
	asked := map[string]int{}
	departed := func(_ context.Context, node string) (bool, error) {
		asked[node]++
		switch node {
		case "node-gone":
			return true, nil
		case "node-unknown":
			return false, errors.New("heartbeat bucket unreachable")
		default:
			return false, nil
		}
	}

	closed, err := svc.NewUnpublishObligationCloser(departed).Sweep(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 2, closed)
	remaining := []string{}
	for _, o := range openObligations(t, svc) {
		remaining = append(remaining, o.NodeID)
	}
	assert.ElementsMatch(t, []string{"node-down", "node-unknown"}, remaining,
		"a node that is down, or whose membership cannot be read, keeps its obligations")
	assert.Equal(t, 1, asked["node-gone"], "membership is asked once per node per sweep")
}
