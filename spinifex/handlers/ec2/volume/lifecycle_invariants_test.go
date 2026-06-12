package handlers_ec2_volume

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRLC1_VolumeDeleteIdempotentOnAbsent enforces the Common Resource
// Lifecycle Contract rule #1 (idempotent delete): deleting an absent volume is
// success, not NotFound, so tofu destroy retries converge. The attached /
// has-snapshots live-reference guards are unaffected — only true absence is
// idempotent.
func TestRLC1_VolumeDeleteIdempotentOnAbsent(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	out, err := svc.DeleteVolume(&ec2.DeleteVolumeInput{
		VolumeId: aws.String("vol-absent00000000"),
	}, "123456789012")

	require.NoErrorf(t, err, "DeleteVolume on an absent volume must return success, not NotFound (RLC rule #1 idempotent delete): return an empty output when GetVolumeConfig reports InvalidVolume.NotFound")
	assert.NotNil(t, out, "DeleteVolume must return a non-nil output on absent (RLC rule #1)")
}
