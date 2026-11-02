package accountlifecycle

//test:in-package — drives the unexported platform reapers and reuses the
// engine test doubles.

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	ec2launchtemplate "github.com/mulgadc/spinifex/spinifex/domains/ec2/launchtemplate"
	ec2placementgroup "github.com/mulgadc/spinifex/spinifex/domains/ec2/placementgroup"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestCurrentBehaviour_AccountTermination_ tests pin what the platform
// reapers report while owner stores still hold records. They describe current
// behaviour, not the target design; a failure means it changed.

const platformBucket = "test-bucket"

// platformOwners runs the real key, placement-group, launch-template and tag
// services behind the subjects the platform reapers call.
type platformOwners struct {
	nc        *nats.Conn
	store     *objectstore.MemoryObjectStore
	keys      *ec2key.KeyServiceImpl
	groups    *ec2placementgroup.PlacementGroupServiceImpl
	templates *ec2launchtemplate.LaunchTemplateServiceImpl
	tags      *ec2tags.TagsServiceImpl
	pgKV      jetstream.KeyValue
	ltKV      jetstream.KeyValue
	tagsKV    jetstream.KeyValue
}

// serveOwner answers subject with fn, taking the account from X-Account-ID
// as the daemon's handlers do.
func serveOwner[I, O any](t *testing.T, nc *nats.Conn, subject string, fn func(context.Context, *I, string) (*O, error)) {
	t.Helper()
	sub, err := nc.QueueSubscribe(subject, "spinifex-workers", func(msg *nats.Msg) {
		natsmsg.ServeNATSRequestCtx(msg, func(ctx context.Context, in *I) (*O, error) {
			return fn(ctx, in, natsmsg.AccountIDFromMsg(msg))
		})
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func newPlatformOwners(t *testing.T) *platformOwners {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	ctx := t.Context()
	o := &platformOwners{nc: nc, store: objectstore.NewMemoryObjectStore()}

	var err error
	o.tagsKV, err = ec2tags.GetOrCreateTagsBucket(ctx, js)
	require.NoError(t, err)
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: platformBucket}}
	o.tags = ec2tags.NewTagsServiceImplWithStore(cfg, o.store, o.tagsKV)
	o.keys = ec2key.NewKeyServiceImplWithStore(o.store, platformBucket)
	o.keys.SetCentralTagStore(o.tags)
	o.groups, err = ec2placementgroup.NewPlacementGroupServiceImplWithNATS(ctx, nil, nc)
	require.NoError(t, err)
	o.groups.SetCentralTagStore(o.tags)
	o.templates, err = ec2launchtemplate.NewLaunchTemplateServiceImplWithNATS(ctx, nil, nc)
	require.NoError(t, err)
	o.templates.SetCentralTagStore(o.tags)
	o.pgKV, err = js.KeyValue(ctx, ec2placementgroup.KVBucketPlacementGroups)
	require.NoError(t, err)
	o.ltKV, err = js.KeyValue(ctx, ec2launchtemplate.KVBucketLaunchTemplates)
	require.NoError(t, err)

	serveOwner(t, nc, "ec2.DescribeKeyPairs", o.keys.DescribeKeyPairs)
	serveOwner(t, nc, "ec2.DeleteKeyPair", o.keys.DeleteKeyPair)
	serveOwner(t, nc, "ec2.DescribePlacementGroups", o.groups.DescribePlacementGroups)
	serveOwner(t, nc, "ec2.DeletePlacementGroup", o.groups.DeletePlacementGroup)
	serveOwner(t, nc, "ec2.DescribeLaunchTemplates", o.templates.DescribeLaunchTemplates)
	serveOwner(t, nc, "ec2.DeleteLaunchTemplate", o.templates.DeleteLaunchTemplate)
	require.NoError(t, nc.Flush())
	return o
}

func (o *platformOwners) objectKeys(t *testing.T) []string {
	t.Helper()
	objects, _, err := objectstore.ListAll(t.Context(), o.store, &s3.ListObjectsV2Input{
		Bucket: aws.String(platformBucket), Prefix: aws.String("keys/" + testAccountID + "/"),
	})
	require.NoError(t, err)
	var keys []string
	for _, obj := range objects {
		keys = append(keys, aws.StringValue(obj.Key))
	}
	return keys
}

func accountKVKeys(t *testing.T, kv jetstream.KeyValue) []string {
	t.Helper()
	keys, err := kv.Keys(t.Context())
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil
	}
	require.NoError(t, err)
	var mine []string
	for _, k := range keys {
		if strings.HasPrefix(k, testAccountID+".") {
			mine = append(mine, k)
		}
	}
	slices.Sort(mine)
	return mine
}

func platformReapers(nc *nats.Conn) []Reaper {
	return []Reaper{&keyPairReaper{nc: nc}, &placementGroupReaper{nc: nc}, &launchTemplateReaper{nc: nc}}
}

// Pins current behaviour that differs from AWS: a teardown reports the
// platform stage drained and deletes the account record while the owner
// stores still hold key, placement-group, launch-template and tag records.
func TestCurrentBehaviour_AccountTermination_PlatformReapersReportDrainedOverHiddenResidue(t *testing.T) {
	o := newPlatformOwners(t)
	ctx := testCtx(t)

	// Live items: each is listed, and deleted through its owner's subject.
	_, err := o.keys.ImportKeyPair(ctx, &ec2.ImportKeyPairInput{
		KeyName:           aws.String("deploy"),
		PublicKeyMaterial: []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"),
	}, testAccountID)
	require.NoError(t, err)
	pg, err := o.groups.CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{
		GroupName: aws.String("spread"), Strategy: aws.String(ec2.PlacementStrategySpread),
	}, testAccountID)
	require.NoError(t, err)
	lt, err := o.templates.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("web"),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{UserData: aws.String(base64.StdEncoding.EncodeToString([]byte("secret")))},
	}, testAccountID)
	require.NoError(t, err)
	pgID, ltID := aws.StringValue(pg.PlacementGroup.GroupId), aws.StringValue(lt.LaunchTemplate.LaunchTemplateId)
	_, err = o.tags.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []*string{aws.String(pgID), aws.String(ltID)},
		Tags:      []*ec2.Tag{{Key: aws.String("team"), Value: aws.String("blue")}},
	}, testAccountID)
	require.NoError(t, err)

	// Residue each owner's Describe skips, as left by older releases or a
	// crash between writes.
	publicKey := "keys/" + testAccountID + "/old-deploy"
	_, err = o.store.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(platformBucket), Key: aws.String(publicKey), Body: strings.NewReader("ssh-ed25519 AAAA"),
	})
	require.NoError(t, err)
	staleTagged := "pg-0stale000000000"
	_, err = o.tags.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []*string{aws.String(staleTagged)}, Tags: []*ec2.Tag{{Key: aws.String("team"), Value: aws.String("blue")}},
	}, testAccountID)
	require.NoError(t, err)
	_, err = o.store.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(platformBucket), Key: aws.String("keys/" + testAccountID + "/key-0badbadbadbadbad0.json"),
		Body: strings.NewReader("{not json"),
	})
	require.NoError(t, err)
	_, err = o.pgKV.Put(ctx, testAccountID+".broken", []byte("{not json"))
	require.NoError(t, err)
	orphanBody := testAccountID + ".lt-0orphan000000000.v1"
	_, err = o.ltKV.Put(ctx, orphanBody, []byte(`{"version_number":1,"data":{"UserData":"c2VjcmV0"}}`))
	require.NoError(t, err)

	accounts := newFakeAccounts()
	result, err := quietEngine(accounts, platformReapers(o.nc)...).Teardown(ctx, request())
	require.NoError(t, err)
	assert.True(t, result.AccountDeleted)
	assert.True(t, accounts.deleted)
	assert.Zero(t, result.StuckCount())

	var deleted []string
	for _, stage := range result.Stages {
		for _, r := range stage.Deleted {
			deleted = append(deleted, r.Kind+"/"+r.ID)
		}
	}
	assert.ElementsMatch(t, []string{"key-pair/deploy", "placement-group/spread", "launch-template/" + ltID}, deleted)

	for _, reaper := range platformReapers(o.nc) {
		found, err := reaper.List(ctx, testAccountID)
		require.NoError(t, err)
		assert.Empty(t, found, "%s lists nothing", reaper.Kind())
	}

	assert.ElementsMatch(t, []string{publicKey, "keys/" + testAccountID + "/key-0badbadbadbadbad0.json"}, o.objectKeys(t))
	assert.Equal(t, []string{testAccountID + ".broken"}, accountKVKeys(t, o.pgKV))
	assert.Equal(t, []string{orphanBody}, accountKVKeys(t, o.ltKV))
	// The owner deletes clear their own tags; only the stale entry remains.
	assert.Equal(t, []string{testAccountID + "." + staleTagged}, accountKVKeys(t, o.tagsKV))
}
