//go:build e2e

package rds

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What the control plane resolves a MariaDB create against. The AMI name is not
// parsed for the engine — these tags are the whole contract.
const (
	mariaAMIName = "spinifex-rds-mariadb"
	mariaEngine  = "mariadb"
	mariaVersion = "11.8"
)

// TestMariaDBSystemImage asserts the CI job's build-and-import step produced an
// image the engine can actually be resolved from. It creates nothing, so it
// takes no DB-VM budget, and it is the cheapest possible failure for a missing
// or mis-tagged AMI: without it the first MariaDB create fails deep in launch
// with an error that names no cause.
func TestMariaDBSystemImage(t *testing.T) {
	t.Parallel()
	f := requireRDSFixture(t)

	out, err := f.AWS.EC2.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("name"),
			Values: []*string{aws.String(mariaAMIName)},
		}},
	})
	require.NoError(t, err, "DescribeImages")
	require.Len(t, out.Images, 1, "expected exactly one %s image in the store", mariaAMIName)

	image := out.Images[0]
	assert.Equal(t, "available", aws.StringValue(image.State))

	tags := make(map[string]string, len(image.Tags))
	for _, tag := range image.Tags {
		tags[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}
	assert.Equal(t, "rds", tags["spinifex:managed-by"])
	assert.Equal(t, mariaEngine, tags["engine"])
	assert.Equal(t, mariaVersion, tags["engine-version"])
	// Without this the control plane withholds the data-volume format grant and
	// every create wedges waiting for a datadir the guest may not format.
	assert.Equal(t, "format-auth-v1", tags["rds-data-volume-contract"])
}
