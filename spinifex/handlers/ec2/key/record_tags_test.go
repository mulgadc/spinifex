package handlers_ec2_key

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The record is read back as ec2.CreateKeyPairOutput, the type it was
// serialised as before keyPairMetadata existed, so this doubles as a canary on
// the stored JSON keys not having moved.
func readKeyPairTags(t *testing.T, svc *KeyServiceImpl, accountID, keyPairID string) map[string]string {
	t.Helper()
	var metadata ec2.CreateKeyPairOutput
	require.NoError(t, json.Unmarshal(readKeyPairRecord(t, svc, accountID, keyPairID), &metadata))
	return filterutil.EC2TagsToMap(metadata.Tags)
}

// readKeyPairRecord returns the stored metadata object verbatim.
func readKeyPairRecord(t *testing.T, svc *KeyServiceImpl, accountID, keyPairID string) []byte {
	t.Helper()
	return readStoredObject(t, svc.store, fmt.Sprintf("keys/%s/%s.json", accountID, keyPairID))
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

// Tagging read-modify-writes the whole record, so a legacy one is rewritten in
// upgraded form. The two halves of that upgrade must travel together: writing
// the normalised fingerprint back without also persisting KeyType would strip
// the only evidence of the key's type and permanently downgrade it to rsa.
func TestKeyPairRecordTagsMirror_UpgradesLegacyRecord(t *testing.T) {
	svc, store := newTestKeyService()

	putStoredObject(t, store, "keys/"+testAccountID+"/key-legacy2.json",
		`{"KeyFingerprint":"`+testLegacyED25519Fingerprint+`","KeyName":"legacy-tagged","KeyPairId":"key-legacy2","Tags":null}`)

	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("key-legacy2")},
		Tags:      []*ec2.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
	}, testAccountID))

	record := string(readKeyPairRecord(t, svc, testAccountID, "key-legacy2"))
	assert.Contains(t, record, `"KeyType":"ed25519"`)
	assert.Contains(t, record, `"KeyFingerprint":"`+testED25519Fingerprint+`"`)

	out, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.KeyPairs, 1)
	assert.Equal(t, "ed25519", *out.KeyPairs[0].KeyType)
	assert.Equal(t, testED25519Fingerprint, *out.KeyPairs[0].KeyFingerprint)
	assert.Equal(t, "prod", filterutil.EC2TagsToMap(out.KeyPairs[0].Tags)["env"])
}

func TestKeyPairRecordTagsMirror_UnownedNoError(t *testing.T) {
	svc, _ := newTestKeyService()
	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("key-missing"), aws.String("pg-other")},
		Tags:      []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testAccountID))
}
