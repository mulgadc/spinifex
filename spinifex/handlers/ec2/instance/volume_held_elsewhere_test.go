package handlers_ec2_instance

//test:in-package — volumeHeldElsewhereError is unexported, and exporting it
//would widen the handler API for a test rather than for a caller.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVolumeHeldElsewhere_NamesTheHolder covers both refusals that mean the
// data is on another node. An operator who gets ServerInternal is told to retry
// something that cannot succeed while that node is down, so the node name and
// the reason have to survive the trip out of the daemon.
func TestVolumeHeldElsewhere_NamesTheHolder(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			name: "live holder, refused by the lease",
			err: errors.New("failed to mount volume: volume is leased by another owner: " +
				"held by node-b since 2026-09-02T17:04:39Z"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := volumeHeldElsewhereError(tc.err)
			require.Error(t, got, "a volume held elsewhere must not fall through to ServerInternal")

			code := awserrors.ValidErrorCodeFromError(got)
			assert.NotEqual(t, awserrors.ErrorServerInternal, code,
				"ServerInternal reads as a transient fault and invites a retry that cannot succeed")
			assert.Contains(t, got.Error(), "node-b", "the caller has to learn where the data is")
		})
	}
}

// TestVolumeHeldElsewhere_IgnoresUnrelatedFailures guards the other direction:
// a genuine internal fault must keep reporting as one rather than being
// relabelled as somebody else's volume.
func TestVolumeHeldElsewhere_IgnoresUnrelatedFailures(t *testing.T) {
	assert.NoError(t, volumeHeldElsewhereError(nil))
	assert.NoError(t, volumeHeldElsewhereError(errors.New("nbd endpoint not ready: timeout waiting for unix socket")))
	assert.NoError(t, volumeHeldElsewhereError(fmt.Errorf("qemu exited: %w", errors.New("no such file"))))

	// A volume whose writes are merely unsealed is opened with a warning rather
	// than refused, so this text is not an exclusion and must not become one.
	assert.NoError(t, volumeHeldElsewhereError(errors.New(
		"took over from node-b, which held unsealed writes since 2026-09-02T17:04:39Z")))
}
