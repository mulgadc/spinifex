package daemon

//test:in-package — the subject is served from the daemon's own subscription
// table, registered by the unexported subscribeAll.

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/internal/testkit/readonlystore"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	ec2launchtemplate "github.com/mulgadc/spinifex/spinifex/domains/ec2/launchtemplate"
	ec2placementgroup "github.com/mulgadc/spinifex/spinifex/domains/ec2/placementgroup"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	inventoryAccount = "111122223333"
	inventoryOther   = "444455556666"
	inventoryEmpty   = "777788889999"
	inventoryBucket  = "test-bucket"
	inventoryPubKey  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	inventoryUser    = "#!/bin/sh\necho tenant-user-data-secret\n"

	orphanTemplateID = "lt-0orphan000000000"
	brokenTemplateID = "lt-0badbadbadbadbad0"
	brokenKeyPairID  = "key-0badbadbadbadbad0"
	deletedGroupID   = "pg-0deleted00000000"
)

// inventoryCluster is a daemon serving its real subscription table over owner
// stores seeded with live records and every residue class, and a separate
// client holding no IAM credential.
type inventoryCluster struct {
	caller  *nats.Conn
	js      jetstream.JetStream
	live    map[string]string
	heldIDs []string
	secrets []string
}

func newInventoryCluster(t *testing.T) *inventoryCluster {
	t.Helper()
	ns, nc, js := testutil.StartTestJetStream(t)
	ctx := t.Context()
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: inventoryBucket}}
	tagsKV, err := ec2tags.GetOrCreateTagsBucket(ctx, js)
	require.NoError(t, err)
	tags := ec2tags.NewTagsServiceImplWithStore(cfg, store, tagsKV)
	keys := ec2key.NewKeyServiceImplWithStore(store, inventoryBucket)
	keys.SetCentralTagStore(tags)
	groups, err := ec2placementgroup.NewPlacementGroupServiceImplWithNATS(ctx, nil, nc)
	require.NoError(t, err)
	templates, err := ec2launchtemplate.NewLaunchTemplateServiceImplWithNATS(ctx, nil, nc)
	require.NoError(t, err)
	c := &inventoryCluster{js: js, live: map[string]string{}}

	tagSpec := func(resourceType string) []*ec2.TagSpecification {
		return []*ec2.TagSpecification{{ResourceType: aws.String(resourceType),
			Tags: []*ec2.Tag{{Key: aws.String("secret-tag-key"), Value: aws.String("secret-tag-value")}}}}
	}
	secretTags := map[string]string{"secret-tag-key": "secret-tag-value"}

	for _, account := range []string{inventoryAccount, inventoryOther} {
		key, err := keys.ImportKeyPair(ctx, &ec2.ImportKeyPairInput{
			KeyName: aws.String("live-key-name"), PublicKeyMaterial: []byte(inventoryPubKey), TagSpecifications: tagSpec("key-pair"),
		}, account)
		require.NoError(t, err)
		group, err := groups.CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{
			GroupName: aws.String("live-group-name"), Strategy: aws.String(ec2.PlacementStrategySpread),
		}, account)
		require.NoError(t, err)
		template, err := templates.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{
			LaunchTemplateName: aws.String("live-template-name"),
			LaunchTemplateData: &ec2.RequestLaunchTemplateData{UserData: aws.String(base64.StdEncoding.EncodeToString([]byte(inventoryUser)))},
			TagSpecifications:  tagSpec("launch-template"),
		}, account)
		require.NoError(t, err)
		if account == inventoryAccount {
			c.live[aws.StringValue(key.KeyPairId)] = ec2v1.PlatformKindKeyPair
			c.live[aws.StringValue(group.PlacementGroup.GroupId)] = ec2v1.PlatformKindPlacementGroup
			c.live[aws.StringValue(template.LaunchTemplate.LaunchTemplateId)] = ec2v1.PlatformKindLaunchTemplate
		}
	}

	// A group still holding a launched instance.
	held, err := groups.CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{
		GroupName: aws.String("held-group-name"), Strategy: aws.String(ec2.PlacementStrategyCluster),
	}, inventoryAccount)
	require.NoError(t, err)
	reserved, err := groups.ReserveClusterNode(ctx, &ec2placementgroup.ReserveClusterNodeInput{
		GroupName: "held-group-name", EligibleNodes: []string{"node-1"},
	}, inventoryAccount)
	require.NoError(t, err)
	_, err = groups.FinalizeClusterInstances(ctx, &ec2placementgroup.FinalizeClusterInstancesInput{
		GroupName: "held-group-name", NodeInstances: map[string][]string{reserved.TargetNode: {"i-0held0000000000"}},
	}, inventoryAccount)
	require.NoError(t, err)
	c.heldIDs = []string{aws.StringValue(held.PlacementGroup.GroupId)}

	// Owner-store residue no Describe subject reports.
	prefix := "keys/" + inventoryAccount + "/"
	for objectKey, body := range map[string]string{
		prefix + "orphan-secret-key-name":                    inventoryPubKey + "\n",
		prefix + brokenKeyPairID + ".json":                   `{"KeyName":"broken-secret-key-name","KeyPairId":`,
		"keys/" + inventoryOther + "/orphan-secret-key-name": inventoryPubKey + "\n",
	} {
		_, err := store.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(inventoryBucket), Key: aws.String(objectKey), Body: bytes.NewReader([]byte(body))})
		require.NoError(t, err)
	}
	ltKV, err := js.KeyValue(ctx, ec2launchtemplate.KVBucketLaunchTemplates)
	require.NoError(t, err)
	orphanBody, err := json.Marshal(ec2launchtemplate.LaunchTemplateVersionRec{VersionNumber: 1,
		Data: &ec2.ResponseLaunchTemplateData{UserData: aws.String(base64.StdEncoding.EncodeToString([]byte(inventoryUser)))}})
	require.NoError(t, err)
	orphanName := inventoryAccount + ".name." + hex.EncodeToString([]byte("orphan-secret-template-name"))
	for kvKey, body := range map[string][]byte{
		inventoryAccount + "." + orphanTemplateID + ".v1": orphanBody,
		inventoryAccount + "." + orphanTemplateID + ".v2": orphanBody,
		orphanName: []byte(orphanTemplateID),
		inventoryAccount + "." + brokenTemplateID:         []byte(`{"launch_template_name":"broken-secret-template-name"`),
		inventoryAccount + "." + brokenTemplateID + ".v1": orphanBody,
	} {
		_, err := ltKV.Put(ctx, kvKey, body)
		require.NoError(t, err)
	}
	// Tag-index entries a placement-group and launch-template delete leave.
	require.NoError(t, tags.PutResourceTags(ctx, inventoryAccount, deletedGroupID, secretTags))
	require.NoError(t, tags.PutResourceTags(ctx, inventoryAccount, orphanTemplateID, secretTags))
	require.NoError(t, tags.PutResourceTags(ctx, inventoryAccount, brokenTemplateID, secretTags))

	c.secrets = []string{
		"i-0held0000000000",
		"live-key-name", "live-group-name", "live-template-name", "held-group-name",
		"orphan-secret-key-name", "broken-secret-key-name", "orphan-secret-template-name",
		hex.EncodeToString([]byte("orphan-secret-template-name")), "broken-secret-template-name",
		"secret-tag-key", "secret-tag-value", "AAAAC3NzaC1lZDI1NTE5", "tenant-user-data-secret",
		base64.StdEncoding.EncodeToString([]byte(inventoryUser)), brokenKeyPairID, brokenTemplateID,
		"keys/", ".json", ".name.", ".v1", ".v2", inventoryAccount + ".", "invalid character", "unexpected end",
	}

	// The daemon reads the object store and tag index through read-only views.
	d := newSubscribeTestDaemon(t, nc, "", "")
	ro := readonlystore.NewObjectStore(t, store)
	d.keyService = ec2key.NewKeyServiceImplWithStore(ro, inventoryBucket)
	d.tagsService = ec2tags.NewTagsServiceImplWithStore(cfg, ro, readonlystore.NewKV(t, tagsKV))
	d.placementGroupService = groups
	d.launchTemplateService = templates
	require.NoError(t, d.subscribeAll())
	// The caller is a separate connection, so the server must hold the SUBs
	// before it publishes or the request can see no responders.
	require.NoError(t, nc.Flush())

	c.caller, err = nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(c.caller.Close)
	return c
}

// request sends body with X-Account-ID set to header and returns the raw reply.
func (c *inventoryCluster) request(t *testing.T, body any, header string) []byte {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	reply, err := natsRequestAs(c.caller, ec2v1.PlatformInventorySubject, data, header, 5*time.Second)
	require.NoError(t, err)
	return reply.Data
}

func (c *inventoryCluster) lastSeq(t *testing.T, bucket string) uint64 {
	t.Helper()
	stream, err := c.js.Stream(t.Context(), "KV_"+bucket)
	require.NoError(t, err)
	info, err := stream.Info(t.Context())
	require.NoError(t, err)
	return info.State.LastSeq
}

// The daemon answers for the account in the body, whatever X-Account-ID says,
// with live and tag-index residue IDs, owner-store residue as counts, and no
// write to any store.
func TestPlatformInventorySubjectReportsTheAccountsOwnerStores(t *testing.T) {
	c := newInventoryCluster(t)
	pgSeq := c.lastSeq(t, ec2placementgroup.KVBucketPlacementGroups)
	ltSeq := c.lastSeq(t, ec2launchtemplate.KVBucketLaunchTemplates)

	raw := c.request(t, ec2v1.PlatformInventoryRequest{AccountID: inventoryAccount, CorrelationRef: "req-1"}, inventoryOther)
	require.Empty(t, replyErrCode(t, raw), string(raw))
	var reply ec2v1.PlatformInventoryReply
	require.NoError(t, json.Unmarshal(raw, &reply))

	assert.Equal(t, inventoryAccount, reply.AccountID)
	assert.Equal(t, "req-1", reply.CorrelationRef)
	want := []ec2v1.PlatformItem{
		{ResourceID: c.heldIDs[0], Kind: ec2v1.PlatformKindPlacementGroup, Disposition: ec2v1.PlatformDispositionDependencyHeld},
		{ResourceID: deletedGroupID, Kind: ec2v1.PlatformKindTagIndex, Disposition: ec2v1.PlatformDispositionResidue},
		{ResourceID: orphanTemplateID, Kind: ec2v1.PlatformKindTagIndex, Disposition: ec2v1.PlatformDispositionResidue},
	}
	for id, kind := range c.live {
		want = append(want, ec2v1.PlatformItem{ResourceID: id, Kind: kind, Disposition: ec2v1.PlatformDispositionLive})
	}
	assert.ElementsMatch(t, want, reply.Items)
	assert.ElementsMatch(t, []ec2v1.PlatformRecordCount{
		{Kind: ec2v1.PlatformKindKeyPair, Disposition: ec2v1.PlatformDispositionUnreadable, Class: ec2v1.PlatformClassUnreadableRecord, Count: 1},
		{Kind: ec2v1.PlatformKindKeyPair, Disposition: ec2v1.PlatformDispositionUnreadable, Class: ec2v1.PlatformClassPublicKeyUnresolved, Count: 1},
		{Kind: ec2v1.PlatformKindLaunchTemplate, Disposition: ec2v1.PlatformDispositionUnreadable, Class: ec2v1.PlatformClassUnreadableRecord, Count: 1},
		{Kind: ec2v1.PlatformKindLaunchTemplate, Disposition: ec2v1.PlatformDispositionResidue, Class: ec2v1.PlatformClassVersionBodyWithoutHeader, Count: 2},
		{Kind: ec2v1.PlatformKindLaunchTemplate, Disposition: ec2v1.PlatformDispositionResidue, Class: ec2v1.PlatformClassNameIndexWithoutHeader, Count: 1},
	}, reply.Counts)
	require.Len(t, reply.Kinds, 4)
	for _, report := range reply.Kinds {
		assert.Equal(t, ec2v1.PlatformStatusRecordsFound, report.Status, report.Kind)
		assert.False(t, report.ScanStartedAt.IsZero(), report.Kind)
		assert.False(t, report.ScanEndedAt.Before(report.ScanStartedAt), report.Kind)
	}

	for _, secret := range c.secrets {
		assert.NotContains(t, string(raw), secret, "the reply discloses %q", secret)
	}
	assert.Equal(t, pgSeq, c.lastSeq(t, ec2placementgroup.KVBucketPlacementGroups), "placement-group bucket written")
	assert.Equal(t, ltSeq, c.lastSeq(t, ec2launchtemplate.KVBucketLaunchTemplates), "launch-template bucket written")
}

// An account with no records is observed clean for every kind, and another
// account's records never appear.
func TestPlatformInventorySubjectReportsAnEmptyAccountClean(t *testing.T) {
	c := newInventoryCluster(t)
	var reply ec2v1.PlatformInventoryReply
	require.NoError(t, json.Unmarshal(c.request(t, ec2v1.PlatformInventoryRequest{AccountID: inventoryEmpty}, inventoryAccount), &reply))

	assert.Empty(t, reply.Items)
	assert.Empty(t, reply.Counts)
	require.Len(t, reply.Kinds, 4)
	for _, report := range reply.Kinds {
		assert.Equal(t, ec2v1.PlatformStatusObservedClean, report.Status, report.Kind)
	}
}

// A public-key object with no metadata, where no metadata is unreadable, is
// residue; the other account's records stay out of the reply.
func TestPlatformInventorySubjectCountsAnOrphanPublicKeyAsResidue(t *testing.T) {
	c := newInventoryCluster(t)
	raw := c.request(t, ec2v1.PlatformInventoryRequest{AccountID: inventoryOther}, inventoryAccount)
	var reply ec2v1.PlatformInventoryReply
	require.NoError(t, json.Unmarshal(raw, &reply))

	assert.Equal(t, []ec2v1.PlatformRecordCount{{
		Kind: ec2v1.PlatformKindKeyPair, Disposition: ec2v1.PlatformDispositionResidue,
		Class: ec2v1.PlatformClassPublicKeyWithoutMetadata, Count: 1,
	}}, reply.Counts)
	require.Len(t, reply.Items, 3)
	for _, item := range reply.Items {
		assert.Equal(t, ec2v1.PlatformDispositionLive, item.Disposition)
		assert.NotContains(t, c.live, item.ResourceID)
	}
	for _, id := range append(c.heldIDs, deletedGroupID, orphanTemplateID) {
		assert.NotContains(t, string(raw), id)
	}
}

// A request without a valid account in its body is refused, not scanned.
func TestPlatformInventorySubjectRefusesARequestWithoutAnAccount(t *testing.T) {
	c := newInventoryCluster(t)
	assert.Equal(t, awserrors.ErrorMissingParameter, replyErrCode(t, c.request(t, ec2v1.PlatformInventoryRequest{}, inventoryAccount)))
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, replyErrCode(t, c.request(t, ec2v1.PlatformInventoryRequest{AccountID: "1111"}, "")))
}

// A daemon that does not serve the subject leaves the request with no responder.
func TestPlatformInventorySubjectHasNoResponderWithoutTheDaemon(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	_, err := natsRequestAs(nc, ec2v1.PlatformInventorySubject, []byte(`{"account_id":"111122223333"}`), "", time.Second)
	require.ErrorIs(t, err, nats.ErrNoResponders)
}
