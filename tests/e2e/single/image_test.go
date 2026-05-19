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
	require.NotEmpty(t, fix.Arch, "Phase 2 must populate fix.Arch before Phase 4")

	// Bootstrap-install.sh has already imported the staged ubuntu cloud
	// image via `--file` (v6+ gold stages ubuntu-26.04, v3 stages
	// ubuntu-24.04). DescribeImages by canonical name is enough — running
	// `spx admin images import --name` here triggers an 800MB+ catalog
	// download that outlasts the job timeout, and `--file` would need a
	// path readable by the test-binary user (different from the user
	// bootstrap-install.sh ran as). Drop the 24.04 candidate once the v3
	// gold image is fully retired.
	candidates := []string{
		"ami-ubuntu-26.04-" + fix.Arch,
		"ami-ubuntu-24.04-" + fix.Arch,
	}
	var resolvedName, resolvedID string
	for _, name := range candidates {
		harness.Detail(t, "image_candidate", name, "arch", fix.Arch)
		harness.Step(t, "describe-images filter name=%s", name)
		out, err := fix.AWS.EC2.DescribeImages(&ec2.DescribeImagesInput{
			Filters: []*ec2.Filter{
				{Name: aws.String("name"), Values: []*string{aws.String(name)}},
			},
		})
		if err == nil && len(out.Images) > 0 {
			resolvedName = name
			resolvedID = aws.StringValue(out.Images[0].ImageId)
			break
		}
	}
	require.NotEmptyf(t, resolvedID,
		"no Ubuntu AMI found via DescribeImages — bootstrap-install.sh did not stage one (tried: %v)", candidates)
	require.NotEmpty(t, resolvedName, "resolved name went empty")
	harness.Detail(t, "image", resolvedName, "ami", resolvedID)
	fix.AMIID = resolvedID

	harness.Step(t, "describe-images by AMI ID (verify round-trip)")
	byID, err := fix.AWS.EC2.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String(fix.AMIID)},
	})
	require.NoErrorf(t, err, "describe-images %s", fix.AMIID)
	require.Lenf(t, byID.Images, 1, "expected exactly 1 image for %s, got %d", fix.AMIID, len(byID.Images))
	require.Equal(t, fix.AMIID, aws.StringValue(byID.Images[0].ImageId), "round-trip AMI ID mismatch")
	require.Equal(t, resolvedName, aws.StringValue(byID.Images[0].Name), "lookup-by-ID returned a different Name than lookup-by-Name")
}
