package cluster

import (
	"net"
	"strconv"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/agents/eks/podidentity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Both webhooks run on every control-plane VM. A shared port makes the second
// listener fail with "address already in use" and respawn forever.
func TestPodIdentityWebhookPortDoesNotCollideWithTokenWebhookReadiness(t *testing.T) {
	_, port, err := net.SplitHostPort(podidentity.DefaultAddr)
	require.NoError(t, err)
	assert.NotEqual(t, strconv.FormatInt(webhookReadyPort, 10), port,
		"pod identity webhook must not listen on the token webhook readiness port")
}
