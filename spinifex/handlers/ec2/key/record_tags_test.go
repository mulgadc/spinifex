package handlers_ec2_key

import (
	"encoding/json"
	"fmt"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readKeyPairTags(t *testing.T, svc *KeyServiceImpl, accountID, keyPairID string) map[string]string {
	t.Helper()
	result, err := svc.store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(fmt.Sprintf("keys/%s/%s.json", accountID, keyPairID)),
	})
	require.NoError(t, err)
	defer result.Body.Close()
	body, err := io.ReadAll(result.Body)
	require.NoError(t, err)
	var metadata ec2.CreateKeyPairOutput
	require.NoError(t, json.Unmarshal(body, &metadata))
	return filterutil.EC2TagsToMap(metadata.Tags)
}

func TestKeyPairRecordTagsMirror(t *testing.T) {
	svc, _ := newTestKeyService()
	out := importTestKey(t, svc, "tag-mirror-key")
	keyPairID := *out.KeyPairId

	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(keyPairID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("yes")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testAccountID))

	tags := readKeyPairTags(t, svc, testAccountID, keyPairID)
	assert.Equal(t, "yes", tags["keep"])
	assert.Equal(t, "v", tags["drop"])

	// Value-mismatched delete is a no-op; matched delete removes.
	require.NoError(t, svc.RemoveRecordTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String(keyPairID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("wrong")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testAccountID))

	tags = readKeyPairTags(t, svc, testAccountID, keyPairID)
	assert.Equal(t, "yes", tags["keep"])
	_, ok := tags["drop"]
	assert.False(t, ok)
}

func TestKeyPairRecordTagsMirror_CrossAccountNoMutation(t *testing.T) {
	svc, _ := newTestKeyService()
	out := importTestKey(t, svc, "tag-guard-key")
	keyPairID := *out.KeyPairId

	// Another account's mirror misses under its own prefix and no-ops.
	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(keyPairID)},
		Tags:      []*ec2.Tag{{Key: aws.String("evil"), Value: aws.String("1")}},
	}, "999999999999"))

	tags := readKeyPairTags(t, svc, testAccountID, keyPairID)
	assert.Empty(t, tags)
}

func TestKeyPairRecordTagsMirror_UnownedNoError(t *testing.T) {
	svc, _ := newTestKeyService()
	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("key-missing"), aws.String("pg-other")},
		Tags:      []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testAccountID))
}
