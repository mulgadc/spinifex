package handlers_ec2_tags_test

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	handlers_ec2_tags "github.com/mulgadc/spinifex/spinifex/handlers/ec2/tags"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAccountID     = "111111111111"
	testED25519PubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
)

func newTagsService(t *testing.T) *handlers_ec2_tags.TagsServiceImpl {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	kv, err := handlers_ec2_tags.GetOrCreateTagsBucket(t.Context(), testutil.NewJetStream(t, nc))
	require.NoError(t, err)
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	return handlers_ec2_tags.NewTagsServiceImplWithStore(cfg, objectstore.NewMemoryObjectStore(), kv)
}

func keyPairTagSpec(tags map[string]string) []*ec2.TagSpecification {
	spec := &ec2.TagSpecification{ResourceType: aws.String("key-pair")}
	for k, v := range tags {
		spec.Tags = append(spec.Tags, &ec2.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return []*ec2.TagSpecification{spec}
}

func describeKeyPairTags(t *testing.T, svc *handlers_ec2_tags.TagsServiceImpl) map[string]map[string]string {
	t.Helper()
	out, err := svc.DescribeTags(t.Context(), &ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{{Name: aws.String("resource-type"), Values: []*string{aws.String("key-pair")}}},
	}, testAccountID)
	require.NoError(t, err)
	got := map[string]map[string]string{}
	for _, td := range out.Tags {
		if got[*td.ResourceId] == nil {
			got[*td.ResourceId] = map[string]string{}
		}
		got[*td.ResourceId][*td.Key] = *td.Value
	}
	return got
}

// AWS lists the TagSpecifications tags of a created or imported key pair in
// DescribeTags under resource type key-pair, and drops them when it is deleted.
func TestKeyPairCreationTags_VisibleToDescribeTags(t *testing.T) {
	tagsSvc := newTagsService(t)
	keySvc := ec2key.NewKeyServiceImplWithStore(objectstore.NewMemoryObjectStore(), "test-bucket")
	keySvc.SetCentralTagStore(tagsSvc)

	created, err := keySvc.CreateKeyPair(t.Context(), &ec2.CreateKeyPairInput{
		KeyName:           aws.String("created"),
		TagSpecifications: keyPairTagSpec(map[string]string{"Name": "created", "awsdiff": "1"}),
	}, testAccountID)
	require.NoError(t, err)

	imported, err := keySvc.ImportKeyPair(t.Context(), &ec2.ImportKeyPairInput{
		KeyName:           aws.String("imported"),
		PublicKeyMaterial: []byte(testED25519PubKey),
		TagSpecifications: keyPairTagSpec(map[string]string{"Name": "imported"}),
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, map[string]map[string]string{
		*created.KeyPairId:  {"Name": "created", "awsdiff": "1"},
		*imported.KeyPairId: {"Name": "imported"},
	}, describeKeyPairTags(t, tagsSvc))

	_, err = keySvc.DeleteKeyPair(t.Context(), &ec2.DeleteKeyPairInput{KeyName: aws.String("created")}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, map[string]map[string]string{
		*imported.KeyPairId: {"Name": "imported"},
	}, describeKeyPairTags(t, tagsSvc))
}
