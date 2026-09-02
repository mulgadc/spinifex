package arn_test

import (
	"testing"
	"uuid"

	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatEKS(t *testing.T) {
	const (
		region  = "ap-southeast-2"
		account = "123456789012"
	)

	assert.Equal(t, "arn:aws:eks:ap-southeast-2:123456789012:cluster/prod",
		arn.FormatEKSCluster(region, account, "prod"))
	assert.Equal(t, "arn:aws:eks:ap-southeast-2:123456789012:nodegroup/prod/workers/abc-123",
		arn.FormatEKSNodegroup(region, account, "prod", "workers", "abc-123"))
	assert.Equal(t, "arn:aws:eks:ap-southeast-2:123456789012:addon/prod/coredns",
		arn.FormatEKSAddon(region, account, "prod", "coredns"))
	assert.Equal(t, "arn:aws:eks:ap-southeast-2:123456789012:access-entry/prod/9f86d081",
		arn.FormatEKSAccessEntry(region, account, "prod", "9f86d081"))
	assert.Equal(t, "arn:aws:eks:ap-southeast-2:123456789012:cluster/prod",
		arn.FormatEKSResource(region, account, "cluster/prod"))
}

// The policy gate rebuilds a nodegroup ARN from identifiers alone and expects
// byte equality with the one the create path persisted, so the derivation has
// to be stable, sensitive to every input, and shaped like the UUID AWS emits.
func TestEKSNodegroupDiscriminator(t *testing.T) {
	const (
		account = "123456789012"
		cluster = "prod"
		name    = "workers"
	)
	got := arn.EKSNodegroupDiscriminator(account, cluster, name)

	assert.Equal(t, got, arn.EKSNodegroupDiscriminator(account, cluster, name))
	assert.NotEqual(t, got, arn.EKSNodegroupDiscriminator("999988887777", cluster, name))
	assert.NotEqual(t, got, arn.EKSNodegroupDiscriminator(account, "dev", name))
	assert.NotEqual(t, got, arn.EKSNodegroupDiscriminator(account, cluster, "batch"))

	// Concatenating the inputs without a separator would collide these two.
	assert.NotEqual(t,
		arn.EKSNodegroupDiscriminator(account, "prod", "workers"),
		arn.EKSNodegroupDiscriminator(account, "prodwork", "ers"))

	parsed, err := uuid.Parse(got)
	require.NoError(t, err)
	assert.Equal(t, got, parsed.String())
}

// A name that needs percent-decoding in a URL path is placed in the ARN
// decoded: the gate must name the object the handler acts on.
func TestFormatEKSPreservesDecodedNames(t *testing.T) {
	assert.Equal(t, "arn:aws:eks:us-east-1:123456789012:cluster/my cluster",
		arn.FormatEKSCluster("us-east-1", "123456789012", "my cluster"))
}
