package arn_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/stretchr/testify/assert"
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

// A name that needs percent-decoding in a URL path is placed in the ARN
// decoded: the gate must name the object the handler acts on.
func TestFormatEKSPreservesDecodedNames(t *testing.T) {
	assert.Equal(t, "arn:aws:eks:us-east-1:123456789012:cluster/my cluster",
		arn.FormatEKSCluster("us-east-1", "123456789012", "my cluster"))
}
