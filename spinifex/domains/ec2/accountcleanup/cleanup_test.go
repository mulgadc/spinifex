package accountcleanup

//test:in-package — the clock is injected to place residue outside the settle
// window without waiting for it.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	ec2launchtemplate "github.com/mulgadc/spinifex/spinifex/domains/ec2/launchtemplate"
	ec2placementgroup "github.com/mulgadc/spinifex/spinifex/domains/ec2/placementgroup"
	ec2spotinstance "github.com/mulgadc/spinifex/spinifex/domains/ec2/spotinstance"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAccount  = "111122223333"
	otherAccount = "999988887777"
	testBucket   = "test-bucket"
	testPubKey   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	memberID     = "i-0123456789abcdef0"
	staleTagID   = "pg-0stale000000000"
)

var errInjected = errors.New("injected failure")

// faultyStore fails object deletes its predicate selects.
type faultyStore struct {
	*objectstore.MemoryObjectStore

	failDelete func(key string) bool
}

var _ objectstore.ObjectStore = (*faultyStore)(nil)

func (f *faultyStore) DeleteObject(ctx context.Context, in *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	if f.failDelete != nil && f.failDelete(aws.StringValue(in.Key)) {
		return nil, errInjected
	}
	return f.MemoryObjectStore.DeleteObject(ctx, in)
}

// owners are the real EC2 owner services over an embedded JetStream, wired to
// the tag index as the daemon wires them.
type owners struct {
	store     *faultyStore
	keys      *ec2key.KeyServiceImpl
	groups    *ec2placementgroup.PlacementGroupServiceImpl
	templates *ec2launchtemplate.LaunchTemplateServiceImpl
	tags      *ec2tags.TagsServiceImpl
	spot      *ec2spotinstance.SpotInstanceServiceImpl
	instances *fakeInstances
	js        jetstream.JetStream
}

// fakeInstances stands in for the cluster-wide instance listing, which needs
// every daemon to answer; its own refusal is covered separately.
type fakeInstances struct {
	refs *References
	err  error
}

func (f *fakeInstances) AccountInstanceReferences(context.Context, string) (*References, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.refs, nil
}

func newOwners(t *testing.T) *owners {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	o := &owners{store: &faultyStore{MemoryObjectStore: objectstore.NewMemoryObjectStore()}}
	tagsKV, err := ec2tags.GetOrCreateTagsBucket(t.Context(), js)
	require.NoError(t, err)
	o.tags = ec2tags.NewTagsServiceImplWithStore(&config.Config{Predastore: config.PredastoreConfig{Bucket: testBucket}}, o.store, tagsKV)
	o.keys = ec2key.NewKeyServiceImplWithStore(o.store, testBucket)
	o.keys.SetCentralTagStore(o.tags)
	o.groups, err = ec2placementgroup.NewPlacementGroupServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	o.groups.SetCentralTagStore(o.tags)
	o.templates, err = ec2launchtemplate.NewLaunchTemplateServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	o.templates.SetCentralTagStore(o.tags)
	o.spot, err = ec2spotinstance.NewSpotInstanceServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	o.instances = &fakeInstances{refs: newReferences()}
	o.js = js
	return o
}

// readers are the real spot and launch-template checks and the instance fake.
func (o *owners) readers() ReferenceReaders {
	return ReferenceReaders{Instances: o.instances, Spot: o.spot, Templates: o.templates}
}

func (o *owners) newCleanup() *Cleanup {
	return New(o.keys, o.groups, o.templates, o.tags, o.readers())
}

// cleanup runs over the owners with residue of any age treated as settled.
func (o *owners) cleanup(t *testing.T) *Result {
	t.Helper()
	return run(t, o.newCleanup())
}

func run(t *testing.T, c *Cleanup) *Result {
	t.Helper()
	c.now = func() time.Time { return time.Now().Add(time.Hour) }
	res, err := c.CleanupPlatform(t.Context(), &Request{AccountID: testAccount, CorrelationRef: "ref-1"})
	require.NoError(t, err)
	return res
}

func (o *owners) importKey(t *testing.T, account, name string) string {
	t.Helper()
	out, err := o.keys.ImportKeyPair(t.Context(), &ec2.ImportKeyPairInput{
		KeyName: aws.String(name), PublicKeyMaterial: []byte(testPubKey),
		TagSpecifications: []*ec2.TagSpecification{{ResourceType: aws.String("key-pair"), Tags: secretTags()}},
	}, account)
	require.NoError(t, err)
	return aws.StringValue(out.KeyPairId)
}

func (o *owners) createGroup(t *testing.T, account, name string) string {
	t.Helper()
	out, err := o.groups.CreatePlacementGroup(t.Context(), &ec2.CreatePlacementGroupInput{
		GroupName: aws.String(name), Strategy: aws.String(ec2.PlacementStrategyCluster),
	}, account)
	require.NoError(t, err)
	id := aws.StringValue(out.PlacementGroup.GroupId)
	o.tag(t, account, id)
	return id
}

func (o *owners) addMember(t *testing.T, group string) {
	t.Helper()
	reserved, err := o.groups.ReserveClusterNode(t.Context(), &ec2placementgroup.ReserveClusterNodeInput{
		GroupName: group, EligibleNodes: []string{"node-1"},
	}, testAccount)
	require.NoError(t, err)
	_, err = o.groups.FinalizeClusterInstances(t.Context(), &ec2placementgroup.FinalizeClusterInstancesInput{
		GroupName: group, NodeInstances: map[string][]string{reserved.TargetNode: {memberID}},
	}, testAccount)
	require.NoError(t, err)
}

func (o *owners) removeMember(t *testing.T, group string) {
	t.Helper()
	_, err := o.groups.RemoveInstance(t.Context(), &ec2placementgroup.RemoveInstanceInput{
		GroupName: group, NodeName: "node-1", InstanceID: memberID,
	}, testAccount)
	require.NoError(t, err)
}

func (o *owners) createTemplate(t *testing.T, account, name string) string {
	t.Helper()
	out, err := o.templates.CreateLaunchTemplate(t.Context(), &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String(name),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			ImageId:  aws.String("ami-123"),
			UserData: aws.String(base64.StdEncoding.EncodeToString([]byte("secret-user-data"))),
		},
	}, account)
	require.NoError(t, err)
	id := aws.StringValue(out.LaunchTemplate.LaunchTemplateId)
	o.tag(t, account, id)
	return id
}

func secretTags() []*ec2.Tag {
	return []*ec2.Tag{{Key: aws.String("secret-tag-key"), Value: aws.String("secret-tag-value")}}
}

func (o *owners) tag(t *testing.T, account, id string) {
	t.Helper()
	_, err := o.tags.CreateTags(t.Context(), &ec2.CreateTagsInput{Resources: []*string{aws.String(id)}, Tags: secretTags()}, account)
	require.NoError(t, err)
}

func (o *owners) putObject(t *testing.T, key, body string) {
	t.Helper()
	_, err := o.store.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(testBucket), Key: aws.String("keys/" + testAccount + "/" + key), Body: strings.NewReader(body),
	})
	require.NoError(t, err)
}

func statuses(res *Result) map[string]string {
	out := map[string]string{}
	for _, k := range res.Kinds {
		out[k.Kind] = k.Status
	}
	return out
}

func allStatus(status string) map[string]string {
	return map[string]string{
		ec2v1.PlatformKindKeyPair: status, ec2v1.PlatformKindPlacementGroup: status,
		ec2v1.PlatformKindLaunchTemplate: status, ec2v1.PlatformKindTagIndex: status,
	}
}

func item(id, kind, disposition, reason string) Item {
	return Item{ResourceID: id, Kind: kind, Disposition: disposition, Reason: reason}
}

// One run deletes every unreferenced key pair and empty placement group with
// its tag-index entry, removes settled hidden residue and orphaned tags,
// blocks on a group holding an instance, and leaves another account alone.
// The next run, once the member leaves, converges to clean.
func TestCleanupPlatformDeletesBlocksAndConverges(t *testing.T) {
	o := newOwners(t)
	keyID := o.importKey(t, testAccount, "secret-key-name")
	emptyID := o.createGroup(t, testAccount, "secret-empty-group")
	heldID := o.createGroup(t, testAccount, "secret-held-group")
	o.addMember(t, "secret-held-group")
	o.putObject(t, "secret-orphan-key", testPubKey)
	o.tag(t, testAccount, staleTagID)
	theirKey := o.importKey(t, otherAccount, "theirs")
	o.createGroup(t, otherAccount, "theirs")
	o.createTemplate(t, otherAccount, "theirs")

	res := o.cleanup(t)

	assert.Equal(t, testAccount, res.AccountID)
	assert.Equal(t, "ref-1", res.CorrelationRef)
	assert.ElementsMatch(t, []Item{
		item(keyID, ec2v1.PlatformKindKeyPair, DispositionDeleted, ""),
		item(emptyID, ec2v1.PlatformKindPlacementGroup, DispositionDeleted, ""),
		{ResourceID: heldID, Kind: ec2v1.PlatformKindPlacementGroup, Disposition: DispositionBlocked, Reason: ReasonInstanceMembers, MemberCount: 1},
		item(staleTagID, ec2v1.PlatformKindTagIndex, DispositionDeleted, ""),
	}, res.Items)
	assert.Equal(t, []Count{{
		Kind: ec2v1.PlatformKindKeyPair, Class: ec2v1.PlatformClassPublicKeyWithoutMetadata, Disposition: DispositionDeleted, Count: 1,
	}}, res.Counts)
	// The held group's own tags are its, not residue.
	assert.Equal(t, map[string]string{
		ec2v1.PlatformKindKeyPair:        ec2v1.PlatformStatusObservedClean,
		ec2v1.PlatformKindPlacementGroup: ec2v1.PlatformStatusRecordsFound,
		ec2v1.PlatformKindLaunchTemplate: ec2v1.PlatformStatusObservedClean,
		ec2v1.PlatformKindTagIndex:       ec2v1.PlatformStatusRecordsFound,
	}, statuses(res))

	theirs, err := o.keys.ScanAccount(t.Context(), otherAccount)
	require.NoError(t, err)
	assert.Equal(t, []string{theirKey}, theirs.LiveKeyPairIDs)
	theirTags, err := o.tags.ScanAccountResourceIDs(t.Context(), otherAccount, "key-", "pg-", "lt-")
	require.NoError(t, err)
	assert.Len(t, theirTags, 3)

	o.removeMember(t, "secret-held-group")
	res = o.cleanup(t)
	assert.Equal(t, []Item{item(heldID, ec2v1.PlatformKindPlacementGroup, DispositionDeleted, "")}, res.Items,
		"items finished on an earlier run are not reported again")
	assert.Empty(t, res.Counts)
	assert.Equal(t, allStatus(ec2v1.PlatformStatusObservedClean), statuses(res))

	res = o.cleanup(t)
	assert.Empty(t, res.Items)
	assert.Equal(t, allStatus(ec2v1.PlatformStatusObservedClean), statuses(res))
}

// The result carries IDs, kinds, dispositions, fixed reasons and counts only:
// no names, tags, key material, user data, member instance IDs or error text.
func TestCleanupPlatformResultDisclosesNoTenantData(t *testing.T) {
	o := newOwners(t)
	o.importKey(t, testAccount, "secret-key-name")
	o.createGroup(t, testAccount, "secret-held-group")
	o.addMember(t, "secret-held-group")
	o.createTemplate(t, testAccount, "secret-template-name")
	o.putObject(t, "secret-orphan-key", testPubKey)
	o.putObject(t, "key-0badbadbadbadbad0.json", "{secret-broken")
	failing := o.importKey(t, testAccount, "secret-failing-key")
	o.store.failDelete = func(key string) bool { return strings.HasSuffix(key, failing+".json") }
	o.importKey(t, testAccount, "secret-instance-key")
	o.instances.refs.addKeys("secret-instance-key")
	o.importKey(t, testAccount, "secret-spot-key")
	o.openSpotRequest(t, "sir-0secret000000001", "secret-spot-key", "")
	o.importKey(t, testAccount, "secret-template-key")
	_, err := o.templates.CreateLaunchTemplate(t.Context(), &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("secret-referencing-template"),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{KeyName: aws.String("secret-template-key")},
	}, testAccount)
	require.NoError(t, err)

	body, err := json.Marshal(o.cleanup(t))
	require.NoError(t, err)
	for _, leak := range []string{
		"secret", "AAAAC3Nz", memberID, "node-1", "injected", "sir-",
		base64.StdEncoding.EncodeToString([]byte("secret-user-data")),
	} {
		assert.NotContains(t, string(body), leak)
	}
	assert.Contains(t, string(body), failing, "the IDs themselves are reported")
}

// A delete that fails part way is failed, not deleted; the re-run completes
// the partly deleted key pair and reports it deleted exactly once.
func TestCleanupPlatformInterruptedRunConverges(t *testing.T) {
	o := newOwners(t)
	keyID := o.importKey(t, testAccount, "deploy")
	o.store.failDelete = func(key string) bool { return strings.HasSuffix(key, ".json") }

	res := o.cleanup(t)
	assert.Equal(t, []Item{item(keyID, ec2v1.PlatformKindKeyPair, DispositionFailed, ReasonOwnerDeleteFailed)}, res.Items)
	assert.Equal(t, ec2v1.PlatformStatusRecordsFound, statuses(res)[ec2v1.PlatformKindKeyPair])
	scan, err := o.keys.ScanAccount(t.Context(), testAccount)
	require.NoError(t, err)
	assert.Equal(t, []string{keyID}, scan.LiveKeyPairIDs, "the metadata anchors the unfinished delete")

	o.store.failDelete = nil
	res = o.cleanup(t)
	assert.Equal(t, []Item{item(keyID, ec2v1.PlatformKindKeyPair, DispositionDeleted, "")}, res.Items)
	assert.Equal(t, allStatus(ec2v1.PlatformStatusObservedClean), statuses(res))
}

// Records the owner cannot read are never deleted and stay reported as residue,
// and orphaned tags whose owner may be one of them are left.
func TestCleanupPlatformLeavesUnreadableRecords(t *testing.T) {
	o := newOwners(t)
	o.putObject(t, "key-0badbadbadbadbad0.json", "{not json")
	o.putObject(t, "orphan", testPubKey)

	res := o.cleanup(t)
	assert.Empty(t, res.Items)
	assert.ElementsMatch(t, []Count{
		{Kind: ec2v1.PlatformKindKeyPair, Class: ec2v1.PlatformClassUnreadableRecord, Disposition: DispositionResidue, Reason: ReasonUnreadableRecord, Count: 1},
		{Kind: ec2v1.PlatformKindKeyPair, Class: ec2v1.PlatformClassPublicKeyUnresolved, Disposition: DispositionResidue, Reason: ReasonOwnerRecordsUnresolved, Count: 1},
	}, res.Counts)
	assert.Equal(t, ec2v1.PlatformStatusRecordsFound, statuses(res)[ec2v1.PlatformKindKeyPair])
	scan, err := o.keys.ScanAccount(t.Context(), testAccount)
	require.NoError(t, err)
	assert.Equal(t, []string{"key-0badbadbadbadbad0"}, scan.UnreadableKeyPairIDs)
}

// Residue younger than the settle window may be a create in flight: it is
// reported, not removed.
func TestCleanupPlatformKeepsUnsettledResidue(t *testing.T) {
	o := newOwners(t)
	o.putObject(t, "orphan", testPubKey)
	c := o.newCleanup()

	res, err := c.CleanupPlatform(t.Context(), &Request{AccountID: testAccount})
	require.NoError(t, err)
	assert.Equal(t, []Count{{
		Kind: ec2v1.PlatformKindKeyPair, Class: ec2v1.PlatformClassPublicKeyWithoutMetadata,
		Disposition: DispositionResidue, Reason: ReasonRecentlyWritten, Count: 1,
	}}, res.Counts)
}

// lyingKeys reports every key pair deleted without deleting anything.
type lyingKeys struct{ *ec2key.KeyServiceImpl }

func (l lyingKeys) CleanupAccount(ctx context.Context, account string, _ time.Time, _ func(string) string) (*ec2key.AccountCleanup, error) {
	scan, err := l.ScanAccount(ctx, account)
	if err != nil {
		return nil, err
	}
	return &ec2key.AccountCleanup{DeletedKeyPairIDs: scan.LiveKeyPairIDs}, nil
}

// An owner that reports a delete is not believed: the re-scan still finds the
// record, so it is failed and still present.
func TestCleanupPlatformProvesDeletesByRescan(t *testing.T) {
	o := newOwners(t)
	keyID := o.importKey(t, testAccount, "deploy")

	res := run(t, New(lyingKeys{o.keys}, o.groups, o.templates, o.tags, o.readers()))
	assert.Contains(t, res.Items, item(keyID, ec2v1.PlatformKindKeyPair, DispositionFailed, ReasonStillPresent))
	assert.Equal(t, ec2v1.PlatformStatusRecordsFound, statuses(res)[ec2v1.PlatformKindKeyPair])
}

// failingRescan fails every scan after the first, so the cleanup deletes but
// cannot prove it.
type failingRescan struct {
	*ec2launchtemplate.LaunchTemplateServiceImpl

	scans int
}

func (f *failingRescan) ScanAccount(ctx context.Context, account string) (*ec2launchtemplate.AccountScan, error) {
	if f.scans++; f.scans > 1 {
		return nil, errInjected
	}
	return f.LaunchTemplateServiceImpl.ScanAccount(ctx, account)
}

// A failed re-scan is incomplete, never clean: the template stays blocked, and
// a tag of that kind no reported resource owns stays, as residue.
func TestCleanupPlatformFailedRescanIsIncomplete(t *testing.T) {
	o := newOwners(t)
	ltID := o.createTemplate(t, testAccount, "web")
	o.tag(t, testAccount, "lt-0stale000000000")

	res := run(t, New(o.keys, o.groups, &failingRescan{LaunchTemplateServiceImpl: o.templates}, o.tags, o.readers()))
	assert.Equal(t, []Item{
		item(ltID, ec2v1.PlatformKindLaunchTemplate, DispositionBlocked, ReasonReferenceUnknown),
		item("lt-0stale000000000", ec2v1.PlatformKindTagIndex, DispositionResidue, ReasonOwnerRecordsUnresolved),
	}, res.Items)
	assert.Equal(t, ec2v1.PlatformStatusScanIncomplete, statuses(res)[ec2v1.PlatformKindLaunchTemplate])
	assert.Equal(t, ec2v1.PlatformStatusScanIncomplete, statuses(res)[ec2v1.PlatformKindTagIndex])
	tagged, err := o.tags.ScanAccountResourceIDs(t.Context(), testAccount, "lt-")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"lt-0stale000000000", ltID}, tagged)
}

// failingTags fails every tag-index clear.
type failingTags struct{ *ec2tags.TagsServiceImpl }

func (failingTags) DeleteAllTags(context.Context, string, string) error { return errInjected }

// A tag-index entry that cannot be cleared is failed and the kind keeps
// records, never clean.
func TestCleanupPlatformReportsUnclearedTags(t *testing.T) {
	o := newOwners(t)
	o.tag(t, testAccount, staleTagID)

	res := run(t, New(o.keys, o.groups, o.templates, failingTags{o.tags}, o.readers()))
	assert.Equal(t, []Item{item(staleTagID, ec2v1.PlatformKindTagIndex, DispositionFailed, ReasonOwnerDeleteFailed)}, res.Items)
	assert.Equal(t, ec2v1.PlatformStatusRecordsFound, statuses(res)[ec2v1.PlatformKindTagIndex])
}

// failingCleanup fails the owner cleanup's own listing.
type failingCleanup struct {
	*ec2placementgroup.PlacementGroupServiceImpl
}

func (failingCleanup) CleanupAccount(context.Context, string, func(string, string) string) (*ec2placementgroup.AccountCleanup, error) {
	return nil, errInjected
}

// An owner cleanup that cannot list deletes nothing and is incomplete; the
// group it could not reach is still reported, from the re-scan.
func TestCleanupPlatformOwnerListingFailureIsIncomplete(t *testing.T) {
	o := newOwners(t)
	groupID := o.createGroup(t, testAccount, "web")

	res := run(t, New(o.keys, failingCleanup{o.groups}, o.templates, o.tags, o.readers()))
	assert.Equal(t, ec2v1.PlatformStatusScanIncomplete, statuses(res)[ec2v1.PlatformKindPlacementGroup])
	assert.Contains(t, res.Items, item(groupID, ec2v1.PlatformKindPlacementGroup, DispositionResidue, ReasonAppearedDuringCleanup))
}

func TestCleanupPlatformValidatesTheRequest(t *testing.T) {
	c := New(nil, nil, nil, nil, ReferenceReaders{})
	for name, tc := range map[string]struct {
		req  *Request
		code string
	}{
		"nil":       {nil, awserrors.ErrorMissingParameter},
		"empty":     {&Request{}, awserrors.ErrorMissingParameter},
		"malformed": {&Request{AccountID: "acct-1"}, awserrors.ErrorInvalidParameterValue},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.CleanupPlatform(t.Context(), tc.req)
			require.Error(t, err)
			got, _ := awserrors.ResolveErrorCode(err)
			assert.Equal(t, tc.code, got)
		})
	}
}

func (o *owners) openSpotRequest(t *testing.T, id, keyName, groupName string) {
	t.Helper()
	spec := &ec2.LaunchSpecification{ImageId: aws.String("ami-123"), KeyName: aws.String(keyName)}
	if groupName != "" {
		spec.Placement = &ec2.SpotPlacement{GroupName: aws.String(groupName)}
	}
	_, err := o.spot.PutSpotInstanceRequests(t.Context(), &ec2spotinstance.PutSpotRequestsInput{Requests: []*ec2.SpotInstanceRequest{{
		SpotInstanceRequestId: aws.String(id), State: aws.String(ec2.SpotInstanceStateOpen), LaunchSpecification: spec,
	}}}, testAccount)
	require.NoError(t, err)
}

func (o *owners) putTemplateRecord(t *testing.T, key, value string) {
	t.Helper()
	kv, err := o.js.KeyValue(t.Context(), ec2launchtemplate.KVBucketLaunchTemplates)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), key, []byte(value))
	require.NoError(t, err)
}

// A live launch template is never deleted: an EKS nodegroup may name it and
// EC2 cannot read EKS records. It stays blocked, described and tagged on every
// run; headerless version bodies and names, which nothing can resolve, are
// removed once settled.
func TestCleanupPlatformNeverDeletesLiveLaunchTemplates(t *testing.T) {
	o := newOwners(t)
	ltID := o.createTemplate(t, testAccount, "web")
	o.putTemplateRecord(t, testAccount+".lt-0orphan000000000.v1", `{"version_number":1}`)

	for range 2 {
		res := o.cleanup(t)
		assert.Equal(t, []Item{item(ltID, ec2v1.PlatformKindLaunchTemplate, DispositionBlocked, ReasonReferenceUnknown)}, res.Items)
		assert.Equal(t, ec2v1.PlatformStatusRecordsFound, statuses(res)[ec2v1.PlatformKindLaunchTemplate])
		assert.Equal(t, ec2v1.PlatformStatusRecordsFound, statuses(res)[ec2v1.PlatformKindTagIndex], "the template keeps its tags")
	}
	scan, err := o.templates.ScanAccount(t.Context(), testAccount)
	require.NoError(t, err)
	assert.Equal(t, []string{ltID}, scan.LiveTemplateIDs)
	assert.Zero(t, scan.OrphanVersionBodies)
	out, err := o.templates.DescribeLaunchTemplateVersions(t.Context(), &ec2.DescribeLaunchTemplateVersionsInput{LaunchTemplateId: aws.String(ltID)}, testAccount)
	require.NoError(t, err)
	assert.Len(t, out.LaunchTemplateVersions, 1, "the version body is untouched")
}

// A key pair a non-terminated instance names is blocked and kept, and so is an
// orphan public key the instance names, because IMDS serves it by name. A
// placement group a stopped instance names is blocked though it has no members.
func TestCleanupPlatformBlocksOnInstanceReferences(t *testing.T) {
	o := newOwners(t)
	usedKey := o.importKey(t, testAccount, "in-use")
	freeKey := o.importKey(t, testAccount, "free")
	o.putObject(t, "served-orphan", testPubKey)
	usedGroup := o.createGroup(t, testAccount, "stopped-member")
	o.instances.refs.addKeys("in-use", "served-orphan")
	o.instances.refs.addGroups("stopped-member")

	res := o.cleanup(t)
	assert.ElementsMatch(t, []Item{
		item(usedKey, ec2v1.PlatformKindKeyPair, DispositionBlocked, ReasonInstanceReference),
		item(freeKey, ec2v1.PlatformKindKeyPair, DispositionDeleted, ""),
		item(usedGroup, ec2v1.PlatformKindPlacementGroup, DispositionBlocked, ReasonInstanceReference),
	}, res.Items)
	assert.Equal(t, []Count{{
		Kind: ec2v1.PlatformKindKeyPair, Class: ec2v1.PlatformClassPublicKeyWithoutMetadata,
		Disposition: DispositionBlocked, Reason: ReasonInstanceReference, Count: 1,
	}}, res.Counts)
	scan, err := o.keys.ScanAccount(t.Context(), testAccount)
	require.NoError(t, err)
	assert.Equal(t, []string{usedKey}, scan.LiveKeyPairIDs)
	assert.Equal(t, 1, scan.OrphanPublicKeys)
	material, err := o.keys.GetPublicKeyMaterial(testAccount, "served-orphan")
	require.NoError(t, err)
	assert.Equal(t, testPubKey, material)
}

// An open spot request's launch specification blocks the key pair and the
// placement group it names until the request is cancelled.
func TestCleanupPlatformBlocksOnSpotRequests(t *testing.T) {
	o := newOwners(t)
	keyID := o.importKey(t, testAccount, "spot-key")
	groupID := o.createGroup(t, testAccount, "spot-group")
	o.openSpotRequest(t, "sir-0000000000000001", "spot-key", "spot-group")

	res := o.cleanup(t)
	assert.ElementsMatch(t, []Item{
		item(keyID, ec2v1.PlatformKindKeyPair, DispositionBlocked, ReasonSpotRequestReference),
		item(groupID, ec2v1.PlatformKindPlacementGroup, DispositionBlocked, ReasonSpotRequestReference),
	}, res.Items)

	_, err := o.spot.CancelSpotInstanceRequests(t.Context(), &ec2.CancelSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []*string{aws.String("sir-0000000000000001")},
	}, testAccount)
	require.NoError(t, err)
	res = o.cleanup(t)
	assert.ElementsMatch(t, []Item{
		item(keyID, ec2v1.PlatformKindKeyPair, DispositionDeleted, ""),
		item(groupID, ec2v1.PlatformKindPlacementGroup, DispositionDeleted, ""),
	}, res.Items)
}

// failingTemplates fails the launch-template reference check.
type failingTemplates struct{}

func (failingTemplates) AccountTemplateReferences(context.Context, string) (*ec2launchtemplate.TemplateReferences, error) {
	return nil, errInjected
}

// failingSpot fails the spot reference check.
type failingSpot struct{}

func (failingSpot) AccountLaunchReferences(context.Context, string) (*ec2spotinstance.LaunchReferences, error) {
	return nil, errInjected
}

// A reference check that cannot complete is never read as "no references":
// key pairs, orphan public keys and placement groups are all kept, both kinds
// are incomplete, and launch-template residue, which no check guards, is
// still removed.
func TestCleanupPlatformFailedReferenceCheckDeletesNothing(t *testing.T) {
	for name, build := range map[string]func(o *owners) *Cleanup{
		"instance listing fails": func(o *owners) *Cleanup {
			o.instances.err = errInjected
			return o.newCleanup()
		},
		"spot listing fails": func(o *owners) *Cleanup {
			r := o.readers()
			r.Spot = failingSpot{}
			return New(o.keys, o.groups, o.templates, o.tags, r)
		},
		"launch-template reading fails": func(o *owners) *Cleanup {
			r := o.readers()
			r.Templates = failingTemplates{}
			return New(o.keys, o.groups, o.templates, o.tags, r)
		},
		"instance reader absent": func(o *owners) *Cleanup {
			r := o.readers()
			r.Instances = nil
			return New(o.keys, o.groups, o.templates, o.tags, r)
		},
		"launch-template reader absent": func(o *owners) *Cleanup {
			r := o.readers()
			r.Templates = nil
			return New(o.keys, o.groups, o.templates, o.tags, r)
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := newOwners(t)
			keyID := o.importKey(t, testAccount, "deploy")
			o.putObject(t, "orphan", testPubKey)
			groupID := o.createGroup(t, testAccount, "web")
			o.putTemplateRecord(t, testAccount+".lt-0orphan000000000.v1", `{"version_number":1}`)

			res := run(t, build(o))
			assert.ElementsMatch(t, []Item{
				item(keyID, ec2v1.PlatformKindKeyPair, DispositionBlocked, ReasonReferenceCheckFailed),
				item(groupID, ec2v1.PlatformKindPlacementGroup, DispositionBlocked, ReasonReferenceCheckFailed),
			}, res.Items)
			assert.ElementsMatch(t, []Count{
				{Kind: ec2v1.PlatformKindKeyPair, Class: ec2v1.PlatformClassPublicKeyWithoutMetadata, Disposition: DispositionBlocked, Reason: ReasonReferenceCheckFailed, Count: 1},
				{Kind: ec2v1.PlatformKindLaunchTemplate, Class: ec2v1.PlatformClassVersionBodyWithoutHeader, Disposition: DispositionDeleted, Count: 1},
			}, res.Counts)
			for _, k := range res.Kinds {
				if k.Kind == ec2v1.PlatformKindKeyPair || k.Kind == ec2v1.PlatformKindPlacementGroup {
					assert.Equal(t, ec2v1.PlatformStatusScanIncomplete, k.Status, k.Kind)
					assert.Equal(t, []string{ReasonReferenceCheckFailed}, k.IncompleteReasons, k.Kind)
				}
			}
			keys, err := o.keys.ScanAccount(t.Context(), testAccount)
			require.NoError(t, err)
			assert.Equal(t, []string{keyID}, keys.LiveKeyPairIDs)
			assert.Equal(t, 1, keys.OrphanPublicKeys)
			groups, err := o.groups.ScanAccount(t.Context(), testAccount)
			require.NoError(t, err)
			assert.Len(t, groups.Groups, 1)
		})
	}
}

// A key pair or placement group named by any version of a live launch
// template, by name or group ID, is blocked: RunInstances may resolve any
// version by $Default, $Latest or number. The template itself stays blocked.
func TestCleanupPlatformBlocksOnLaunchTemplateReferences(t *testing.T) {
	o := newOwners(t)
	usedKey := o.importKey(t, testAccount, "template-key")
	freeKey := o.importKey(t, testAccount, "free")
	byName := o.createGroup(t, testAccount, "template-group")
	byID := o.createGroup(t, testAccount, "by-id")
	ltID := o.createTemplate(t, testAccount, "web")
	_, err := o.templates.CreateLaunchTemplateVersion(t.Context(), &ec2.CreateLaunchTemplateVersionInput{
		LaunchTemplateId: aws.String(ltID),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			KeyName:   aws.String("template-key"),
			Placement: &ec2.LaunchTemplatePlacementRequest{GroupName: aws.String("template-group")},
		},
	}, testAccount)
	require.NoError(t, err)
	_, err = o.templates.CreateLaunchTemplateVersion(t.Context(), &ec2.CreateLaunchTemplateVersionInput{
		LaunchTemplateId:   aws.String(ltID),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{Placement: &ec2.LaunchTemplatePlacementRequest{GroupId: aws.String(byID)}},
	}, testAccount)
	require.NoError(t, err)

	res := o.cleanup(t)
	assert.ElementsMatch(t, []Item{
		item(usedKey, ec2v1.PlatformKindKeyPair, DispositionBlocked, ReasonLaunchTemplateReference),
		item(freeKey, ec2v1.PlatformKindKeyPair, DispositionDeleted, ""),
		item(byName, ec2v1.PlatformKindPlacementGroup, DispositionBlocked, ReasonLaunchTemplateReference),
		item(byID, ec2v1.PlatformKindPlacementGroup, DispositionBlocked, ReasonLaunchTemplateReference),
		item(ltID, ec2v1.PlatformKindLaunchTemplate, DispositionBlocked, ReasonReferenceUnknown),
	}, res.Items)
	keys, err := o.keys.ScanAccount(t.Context(), testAccount)
	require.NoError(t, err)
	assert.Equal(t, []string{usedKey}, keys.LiveKeyPairIDs)
	groups, err := o.groups.ScanAccount(t.Context(), testAccount)
	require.NoError(t, err)
	assert.Len(t, groups.Groups, 2)
}

// A live template version the owner cannot decode may name any key pair or
// group, so both kinds are blocked and incomplete and nothing is deleted.
func TestCleanupPlatformUndecodableTemplateVersionDeletesNothing(t *testing.T) {
	o := newOwners(t)
	keyID := o.importKey(t, testAccount, "deploy")
	groupID := o.createGroup(t, testAccount, "web")
	ltID := o.createTemplate(t, testAccount, "web")
	o.putTemplateRecord(t, testAccount+"."+ltID+".v2", "{not json")

	res := o.cleanup(t)
	assert.Contains(t, res.Items, item(keyID, ec2v1.PlatformKindKeyPair, DispositionBlocked, ReasonReferenceCheckFailed))
	assert.Contains(t, res.Items, item(groupID, ec2v1.PlatformKindPlacementGroup, DispositionBlocked, ReasonReferenceCheckFailed))
	assert.Equal(t, ec2v1.PlatformStatusScanIncomplete, statuses(res)[ec2v1.PlatformKindKeyPair])
	assert.Equal(t, ec2v1.PlatformStatusScanIncomplete, statuses(res)[ec2v1.PlatformKindPlacementGroup])
	keys, err := o.keys.ScanAccount(t.Context(), testAccount)
	require.NoError(t, err)
	assert.Equal(t, []string{keyID}, keys.LiveKeyPairIDs)
}

// A tag-index entry whose owner re-scan holds an unreadable record is not
// cleared, because that record may be its owner; it is residue, not deleted.
func TestCleanupPlatformUnresolvedOwnerKeepsTags(t *testing.T) {
	for name, seed := range map[string]func(t *testing.T, o *owners) string{
		"key pair": func(t *testing.T, o *owners) string {
			o.putObject(t, "key-0badbadbadbadbad0.json", "{not json")
			return "key-0stale000000000"
		},
		"placement group": func(t *testing.T, o *owners) string {
			kv, err := o.js.KeyValue(t.Context(), ec2placementgroup.KVBucketPlacementGroups)
			require.NoError(t, err)
			_, err = kv.Put(t.Context(), testAccount+".broken", []byte("{not json"))
			require.NoError(t, err)
			return staleTagID
		},
		"launch template": func(t *testing.T, o *owners) string {
			o.putTemplateRecord(t, testAccount+".lt-0broken000000000", "{not json")
			return "lt-0stale000000000"
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := newOwners(t)
			stale := seed(t, o)
			o.tag(t, testAccount, stale)

			res := o.cleanup(t)
			assert.Contains(t, res.Items, item(stale, ec2v1.PlatformKindTagIndex, DispositionResidue, ReasonOwnerRecordsUnresolved))
			assert.Equal(t, ec2v1.PlatformStatusScanIncomplete, statuses(res)[ec2v1.PlatformKindTagIndex])
			tagged, err := o.tags.ScanAccountResourceIDs(t.Context(), testAccount, stale[:3])
			require.NoError(t, err)
			assert.Equal(t, []string{stale}, tagged)
		})
	}
}

// Settled launch-template residue the owner fails to remove is reported
// failed from the re-scan, never deleted.
func TestCleanupPlatformResidueRemovalFailureIsFailed(t *testing.T) {
	o := newOwners(t)
	o.putTemplateRecord(t, testAccount+".lt-0orphan000000000.v1", `{"version_number":1}`)

	res := run(t, New(o.keys, o.groups, failingResidue{o.templates}, o.tags, o.readers()))
	assert.Equal(t, []Count{{
		Kind: ec2v1.PlatformKindLaunchTemplate, Class: ec2v1.PlatformClassVersionBodyWithoutHeader,
		Disposition: DispositionFailed, Reason: ReasonOwnerDeleteFailed, Count: 1,
	}}, res.Counts)
	assert.Equal(t, ec2v1.PlatformStatusRecordsFound, statuses(res)[ec2v1.PlatformKindLaunchTemplate])
}

// failingResidue reports every settled orphan as a failed removal and removes nothing.
type failingResidue struct {
	*ec2launchtemplate.LaunchTemplateServiceImpl
}

func (f failingResidue) CleanupAccountResidue(ctx context.Context, acct string, _ time.Time) (*ec2launchtemplate.AccountCleanup, error) {
	scan, err := f.ScanAccount(ctx, acct)
	if err != nil {
		return nil, err
	}
	return &ec2launchtemplate.AccountCleanup{KeptTemplateIDs: scan.LiveTemplateIDs, OrphanBodiesFailed: scan.OrphanVersionBodies}, nil
}
