//go:build e2e

package single

import (
	"regexp"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// amiIDRE matches "ami-<hex>" anywhere in spx import output. The CLI prints
// the new AMI ID on a "✅ Image import complete. Image-ID (AMI): ami-..."
// line; capturing the ID alone keeps us tolerant of the surrounding format
// changing (the bash script uses a separate describe-images call entirely
// because it didn't trust the CLI output).
var amiIDRE = regexp.MustCompile(`ami-[0-9a-f]+`)

// phase4_Image imports (or rediscovers) the architecture-appropriate Ubuntu
// AMI staged by bootstrap-install.sh and stashes the ID on the fixture for
// Phase 5+. Maps to run-e2e.sh ~233–255.
func phase4_Image(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 4 — Image Management")
	require.NotEmpty(t, fix.Arch, "Phase 2 must populate fix.Arch before Phase 4")

	// Bash hard-coded the mapping to ubuntu-24.04-{x86_64,arm64}; keep
	// parity. fix.Arch comes from DescribeInstanceTypes so it'll be the
	// canonical AWS arch label.
	imgName := "ubuntu-24.04-" + fix.Arch
	harness.Detail(t, "image", imgName, "arch", fix.Arch)

	harness.Step(t, "spx admin images import --name %s", imgName)
	// wantErr=true: bootstrap-install.sh has already imported this AMI in
	// most environments, so the import returns non-zero on duplicate. We
	// don't care about the exit code — we care about either parsing an
	// AMI ID out of the output or successfully filtering DescribeImages
	// by the canonical ami-<name> tag afterwards.
	out := harness.SpxRun(t, true, "admin", "images", "import", "--name", imgName)

	// Try to parse the AMI ID directly out of the CLI's success line.
	amiID := amiIDRE.FindString(out)
	if amiID != "" {
		harness.Detail(t, "spx_parsed_ami", amiID)
	} else {
		harness.Step(t, "spx import did not yield AMI ID; falling back to describe-images")
	}

	// Either confirm the parsed ID or look it up by name.
	// Bash filters on `Name=name,Values=ami-${IMAGE_NAME}` — the leading
	// `ami-` prefix is part of the AMI's *name* in spinifex (admin.go:487),
	// not the AMI ID. Replicate exactly.
	filterName := "ami-" + imgName
	describeOut, err := fix.AWS.EC2.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("name"), Values: []*string{aws.String(filterName)}},
		},
	})
	require.NoErrorf(t, err, "describe-images filter name=%s", filterName)
	require.NotEmptyf(t, describeOut.Images,
		"no AMI matched name=%s — bootstrap-install.sh did not stage it and the import failed:\n%s",
		filterName, truncate(out, 2000))

	resolvedID := aws.StringValue(describeOut.Images[0].ImageId)
	require.NotEmpty(t, resolvedID, "describe-images returned blank ImageId")

	if amiID != "" && amiID != resolvedID {
		// Parsed an AMI ID from spx output but it doesn't match what
		// describe-images by name returns — surface the mismatch loudly
		// rather than silently preferring one source over the other.
		t.Fatalf("spx output AMI %s != describe-images AMI %s for name %s",
			amiID, resolvedID, filterName)
	}
	fix.AMIID = resolvedID

	harness.Step(t, "describe-images by AMI ID (verify exactly one match)")
	byID, err := fix.AWS.EC2.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String(fix.AMIID)},
	})
	require.NoErrorf(t, err, "describe-images %s", fix.AMIID)
	require.Lenf(t, byID.Images, 1, "expected exactly 1 image for %s, got %d", fix.AMIID, len(byID.Images))
	require.Equal(t, fix.AMIID, aws.StringValue(byID.Images[0].ImageId), "round-trip AMI ID mismatch")

	harness.Detail(t, "ami", fix.AMIID, "name", aws.StringValue(byID.Images[0].Name))
}

// truncate is used to keep failure messages from dumping arbitrarily large
// spx CLI output verbatim into the test log.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}
