package handlers_ec2_key

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func keyPairTagSpecs(resourceType string) []*ec2.TagSpecification {
	return []*ec2.TagSpecification{{
		ResourceType: aws.String(resourceType),
		Tags: []*ec2.Tag{
			{Key: aws.String("Name"), Value: aws.String("build-key")},
			{Key: aws.String("env"), Value: aws.String("dev")},
		},
	}}
}

func TestCreateKeyPair_TagSpecifications(t *testing.T) {
	requireSSHKeygen(t)
	svc, _ := newTestKeyService()

	out, err := svc.CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{
		KeyName:           aws.String("tagged-create-key"),
		TagSpecifications: keyPairTagSpecs("key-pair"),
	}, testAccountID)
	require.NoError(t, err)

	tags := filterutil.EC2TagsToMap(out.Tags)
	assert.Equal(t, "build-key", tags["Name"])
	assert.Equal(t, "dev", tags["env"])

	// Persisted metadata round-trips through DescribeKeyPairs.
	desc, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{
		KeyPairIds: []*string{out.KeyPairId},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.KeyPairs, 1)
	descTags := filterutil.EC2TagsToMap(desc.KeyPairs[0].Tags)
	assert.Equal(t, "build-key", descTags["Name"])
	assert.Equal(t, "dev", descTags["env"])
}

func TestImportKeyPair_TagSpecifications(t *testing.T) {
	svc, _ := newTestKeyService()

	out, err := svc.ImportKeyPair(context.Background(), &ec2.ImportKeyPairInput{
		KeyName:           aws.String("tagged-import-key"),
		PublicKeyMaterial: []byte(testED25519PubKey),
		TagSpecifications: keyPairTagSpecs("key-pair"),
	}, testAccountID)
	require.NoError(t, err)

	tags := filterutil.EC2TagsToMap(out.Tags)
	assert.Equal(t, "build-key", tags["Name"])

	// Tag filter matches the imported key.
	desc, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("tag:env"),
			Values: []*string{aws.String("dev")},
		}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.KeyPairs, 1)
	assert.Equal(t, "tagged-import-key", *desc.KeyPairs[0].KeyName)

	// Non-matching tag filter excludes it.
	desc, err = svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("tag:env"),
			Values: []*string{aws.String("prod")},
		}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.KeyPairs)
}

func TestImportKeyPair_TagSpecificationsWrongType(t *testing.T) {
	svc, _ := newTestKeyService()

	// Tag specs for a different resource type are ignored.
	out, err := svc.ImportKeyPair(context.Background(), &ec2.ImportKeyPairInput{
		KeyName:           aws.String("untagged-import-key"),
		PublicKeyMaterial: []byte(testED25519PubKey),
		TagSpecifications: keyPairTagSpecs("volume"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Tags)
}
