package daemon

//test:in-package — the evidence is the daemon's own subscription table,
// registered by the unexported subscribeAll.

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	ec2launchtemplate "github.com/mulgadc/spinifex/spinifex/domains/ec2/launchtemplate"
	ec2placementgroup "github.com/mulgadc/spinifex/spinifex/domains/ec2/placementgroup"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestCurrentBehaviour_AccountTermination_ tests pin what any NATS client
// can read through the daemon's existing per-kind EC2 subjects today. They
// describe current behaviour, not the target design; a failure means it changed.

const (
	disclosureAccount = "111122223333"
	disclosureOther   = "444455556666"
	disclosureBucket  = "test-bucket"
)

// disclosureCluster is a daemon serving its real subscription table, the
// stores behind it, and a separate NATS client that holds no IAM credential.
type disclosureCluster struct {
	caller *nats.Conn
	store  *objectstore.MemoryObjectStore
	js     jetstream.JetStream
}

func newDisclosureCluster(t *testing.T) *disclosureCluster {
	t.Helper()
	ns, nc, js := testutil.StartTestJetStream(t)
	d := newSubscribeTestDaemon(t, nc, "", "")

	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: disclosureBucket}}
	tagsKV, err := ec2tags.GetOrCreateTagsBucket(t.Context(), js)
	require.NoError(t, err)
	d.keyService = ec2key.NewKeyServiceImplWithStore(store, disclosureBucket)
	d.tagsService = ec2tags.NewTagsServiceImplWithStore(cfg, store, tagsKV)
	d.placementGroupService, err = ec2placementgroup.NewPlacementGroupServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	d.launchTemplateService, err = ec2launchtemplate.NewLaunchTemplateServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	d.wirePlatformTagIndex()
	require.NoError(t, d.subscribeAll())
	// The caller is a separate connection, so the server must hold the SUBs
	// before it publishes or the request can see no responders.
	require.NoError(t, nc.Flush())

	caller, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(caller.Close)
	return &disclosureCluster{caller: caller, store: store, js: js}
}

// call sends one request with X-Account-ID set to account, the only account
// selector the subjects read, and decodes a successful reply into out.
func (c *disclosureCluster) call(t *testing.T, subject, account string, in, out any) {
	t.Helper()
	data, err := json.Marshal(in)
	require.NoError(t, err)
	reply, err := natsRequestAs(c.caller, subject, data, account, 5*time.Second)
	require.NoError(t, err)
	require.Empty(t, replyErrCode(t, reply.Data), "%s answered %s", subject, reply.Data)
	if out != nil {
		require.NoError(t, json.Unmarshal(reply.Data, out))
	}
}

// platformIDs is the account-scoped identity set each Describe subject returns.
type platformIDs struct {
	keyPairs, placementGroups, launchTemplates, taggedResources []string
}

// seededAccount records the identities created for one account.
type seededAccount struct {
	keyName, keyID, groupName, groupID, templateName, templateID string
}

func (c *disclosureCluster) seed(t *testing.T, account, suffix string) seededAccount {
	t.Helper()
	tag := []*ec2.Tag{{Key: aws.String("owner"), Value: aws.String(suffix)}}

	var key ec2.CreateKeyPairOutput
	c.call(t, "ec2.CreateKeyPair", account, &ec2.CreateKeyPairInput{
		KeyName:           aws.String("key-" + suffix),
		TagSpecifications: []*ec2.TagSpecification{{ResourceType: aws.String("key-pair"), Tags: tag}},
	}, &key)
	var pg ec2.CreatePlacementGroupOutput
	c.call(t, "ec2.CreatePlacementGroup", account, &ec2.CreatePlacementGroupInput{
		GroupName: aws.String("pg-" + suffix), Strategy: aws.String(ec2.PlacementStrategySpread),
	}, &pg)
	var lt ec2.CreateLaunchTemplateOutput
	c.call(t, "ec2.CreateLaunchTemplate", account, &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("lt-" + suffix),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{ImageId: aws.String("ami-123")},
	}, &lt)
	s := seededAccount{
		keyName: aws.StringValue(key.KeyName), keyID: aws.StringValue(key.KeyPairId),
		groupName: aws.StringValue(pg.PlacementGroup.GroupName), groupID: aws.StringValue(pg.PlacementGroup.GroupId),
		templateName: aws.StringValue(lt.LaunchTemplate.LaunchTemplateName), templateID: aws.StringValue(lt.LaunchTemplate.LaunchTemplateId),
	}
	c.call(t, "ec2.CreateTags", account, &ec2.CreateTagsInput{
		Resources: []*string{aws.String(s.groupID), aws.String(s.templateID)}, Tags: tag,
	}, nil)
	return s
}

// describe reads every platform identity the four Describe subjects return
// for account, as "<id>/<name>" pairs (tag rows as "<resource-id>").
func (c *disclosureCluster) describe(t *testing.T, account string) platformIDs {
	t.Helper()
	var ids platformIDs

	var keys ec2.DescribeKeyPairsOutput
	c.call(t, "ec2.DescribeKeyPairs", account, &ec2.DescribeKeyPairsInput{}, &keys)
	for _, kp := range keys.KeyPairs {
		ids.keyPairs = append(ids.keyPairs, aws.StringValue(kp.KeyPairId)+"/"+aws.StringValue(kp.KeyName))
	}
	var groups ec2.DescribePlacementGroupsOutput
	c.call(t, "ec2.DescribePlacementGroups", account, &ec2.DescribePlacementGroupsInput{}, &groups)
	for _, g := range groups.PlacementGroups {
		ids.placementGroups = append(ids.placementGroups, aws.StringValue(g.GroupId)+"/"+aws.StringValue(g.GroupName))
	}
	var templates ec2.DescribeLaunchTemplatesOutput
	c.call(t, "ec2.DescribeLaunchTemplates", account, &ec2.DescribeLaunchTemplatesInput{}, &templates)
	for _, lt := range templates.LaunchTemplates {
		ids.launchTemplates = append(ids.launchTemplates, aws.StringValue(lt.LaunchTemplateId)+"/"+aws.StringValue(lt.LaunchTemplateName))
	}
	var tags ec2.DescribeTagsOutput
	c.call(t, "ec2.DescribeTags", account, &ec2.DescribeTagsInput{}, &tags)
	for _, row := range tags.Tags {
		if id := aws.StringValue(row.ResourceId); !slices.Contains(ids.taggedResources, id) {
			ids.taggedResources = append(ids.taggedResources, id)
		}
	}
	for _, s := range [][]string{ids.keyPairs, ids.placementGroups, ids.launchTemplates, ids.taggedResources} {
		slices.Sort(s)
	}
	return ids
}

func sortedIDs(ids ...string) []string {
	slices.Sort(ids)
	return ids
}

// A NATS client with no IAM credential chooses the account in X-Account-ID
// and receives that account's key pair, placement group and launch template
// IDs and names, and every tagged key-, pg- and lt- ID, scoped to it alone.
func TestCurrentBehaviour_AccountTermination_DirectNATSCallerReadsAnyAccountsPlatformIdentities(t *testing.T) {
	c := newDisclosureCluster(t)
	a := c.seed(t, disclosureAccount, "a")
	b := c.seed(t, disclosureOther, "b")

	for _, tc := range []struct {
		account string
		s       seededAccount
	}{{disclosureAccount, a}, {disclosureOther, b}} {
		got := c.describe(t, tc.account)
		assert.Equal(t, []string{tc.s.keyID + "/" + tc.s.keyName}, got.keyPairs, "key pairs of %s", tc.account)
		assert.Equal(t, []string{tc.s.groupID + "/" + tc.s.groupName}, got.placementGroups, "placement groups of %s", tc.account)
		assert.Equal(t, []string{tc.s.templateID + "/" + tc.s.templateName}, got.launchTemplates, "launch templates of %s", tc.account)
		assert.Equal(t, sortedIDs(tc.s.keyID, tc.s.groupID, tc.s.templateID), got.taggedResources, "tag index of %s", tc.account)
	}

	// Without the header the subjects scope to the empty account and return nothing.
	assert.Equal(t, platformIDs{}, c.describe(t, ""))
}

// Records the owner stores hold but Describe skips are not reachable through
// the per-kind subjects: orphan and undecodable key-pair objects, undecodable
// placement-group records, and launch-template bodies or headers with no live template.
func TestCurrentBehaviour_AccountTermination_DirectNATSCallerCannotSeeHiddenResidue(t *testing.T) {
	c := newDisclosureCluster(t)
	c.seed(t, disclosureAccount, "a")
	before := c.describe(t, disclosureAccount)

	prefix := "keys/" + disclosureAccount + "/"
	for key, body := range map[string]string{
		prefix + "orphan-public-key":          "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n",
		prefix + "key-0badbadbadbadbad0.json": "{not json",
	} {
		_, err := c.store.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String(disclosureBucket), Key: aws.String(key), Body: bytes.NewReader([]byte(body)),
		})
		require.NoError(t, err)
	}
	pgKV, err := c.js.KeyValue(t.Context(), ec2placementgroup.KVBucketPlacementGroups)
	require.NoError(t, err)
	_, err = pgKV.Put(t.Context(), disclosureAccount+".broken", []byte("{not json"))
	require.NoError(t, err)
	ltKV, err := c.js.KeyValue(t.Context(), ec2launchtemplate.KVBucketLaunchTemplates)
	require.NoError(t, err)
	for key, body := range map[string]string{
		disclosureAccount + ".lt-0orphan000000000.v1":  `{"version_number":1,"data":{"UserData":"c2VjcmV0"}}`,
		disclosureAccount + ".name.6f727068616e":       "lt-0orphan000000000",
		disclosureAccount + ".lt-0badbadbadbadbad0":    "{not json",
		disclosureAccount + ".lt-0badbadbadbadbad0.v1": `{"version_number":1}`,
	} {
		_, err := ltKV.Put(t.Context(), key, []byte(body))
		require.NoError(t, err)
	}

	after := c.describe(t, disclosureAccount)
	assert.Equal(t, before.keyPairs, after.keyPairs)
	assert.Equal(t, before.placementGroups, after.placementGroups)
	assert.Equal(t, before.launchTemplates, after.launchTemplates)
	assert.Equal(t, before.taggedResources, after.taggedResources)
}

// Deleting a key pair, placement group or launch template clears its
// tag-index entry, so DescribeTags no longer returns its rows, as in AWS.
func TestPlatformDeletesClearTagIndexEntries(t *testing.T) {
	c := newDisclosureCluster(t)
	s := c.seed(t, disclosureAccount, "a")
	before := c.describe(t, disclosureAccount)
	require.Equal(t, sortedIDs(s.keyID, s.groupID, s.templateID), before.taggedResources)

	c.call(t, "ec2.DeleteKeyPair", disclosureAccount, &ec2.DeleteKeyPairInput{KeyName: aws.String(s.keyName)}, nil)
	c.call(t, "ec2.DeletePlacementGroup", disclosureAccount, &ec2.DeletePlacementGroupInput{GroupName: aws.String(s.groupName)}, nil)
	c.call(t, "ec2.DeleteLaunchTemplate", disclosureAccount, &ec2.DeleteLaunchTemplateInput{LaunchTemplateId: aws.String(s.templateID)}, nil)

	got := c.describe(t, disclosureAccount)
	assert.Empty(t, got.keyPairs)
	assert.Empty(t, got.placementGroups)
	assert.Empty(t, got.launchTemplates)
	assert.Empty(t, got.taggedResources)
	var tags ec2.DescribeTagsOutput
	c.call(t, "ec2.DescribeTags", disclosureAccount, &ec2.DescribeTagsInput{}, &tags)
	assert.Empty(t, tags.Tags)
}
