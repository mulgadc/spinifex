//go:build e2e

package single

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// phase4_Image rediscovers the architecture-appropriate Ubuntu AMI that
// bootstrap-install.sh imported from the pre-staged gold-image file
// (~/images/ubuntu-{26,24}.04.img → `spx admin images import --file`) and
// stashes the ID on the fixture for Phase 5+. Maps to run-e2e.sh ~233–255.
func phase4_Image(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 4 — Image Management")

	// Resolve the gold-image AMI via the cluster discovery helper. The first
	// caller pays the DescribeImages + state-poll cost; this and every other
	// AMI consumer hit the memoized ID.
	amiID := needAMI(t, fix)
	require.NotEmpty(t, amiID, "needAMI returned empty AMI ID")
	_, arch := needInstanceTypeArch(t, fix)
	harness.Detail(t, "ami", amiID, "arch", arch)

	harness.Step(t, "describe-images by AMI ID (verify round-trip)")
	byID, err := fix.AWS.EC2.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String(amiID)},
	})
	require.NoErrorf(t, err, "describe-images %s", amiID)
	require.Lenf(t, byID.Images, 1, "expected exactly 1 image for %s, got %d", amiID, len(byID.Images))
	require.Equal(t, amiID, aws.StringValue(byID.Images[0].ImageId), "round-trip AMI ID mismatch")
	require.NotEmpty(t, aws.StringValue(byID.Images[0].Name), "lookup-by-ID returned empty Name")
}
