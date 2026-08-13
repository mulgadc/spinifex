package qemunbdd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLaunchService_ServesRealProviderOverNATS is launchService's one piece
// of real logic: it must root an actual qcow2 Provider at cfg.BaseDir and
// answer capabilities with that provider's own, not a stub's.
func TestLaunchService_ServesRealProviderOverNATS(t *testing.T) {
	ns, client := testutil.StartTestNATS(t)

	cfg := &Config{
		NatsHost: ns.ClientURL(),
		BaseDir:  t.TempDir(),
		NodeName: "test-node",
	}
	go func() { _ = launchService(cfg) }()

	reqBody, err := json.Marshal(ebsprovider.GetCapabilitiesRequest{Versioned: ebsprovider.NewVersioned()})
	require.NoError(t, err)

	var msg *nats.Msg
	require.Eventually(t, func() bool {
		m, reqErr := client.Request(ebsprovider.CapabilitiesSubject, reqBody, time.Second)
		if reqErr != nil {
			return false
		}
		msg = m
		return true
	}, 5*time.Second, 50*time.Millisecond, "qemunbdd never answered %s", ebsprovider.CapabilitiesSubject)

	var resp ebsprovider.GetCapabilitiesResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	require.Nil(t, resp.Error)
	assert.Equal(t, capabilities, resp.Capabilities)
}

// TestLaunchService_ScopesPublishToNodeID confirms cfg.NodeName reaches
// natsserve.Options.NodeID: PublishVolume must be reachable on this node's
// own subject, not the wildcard every-node form.
func TestLaunchService_ScopesPublishToNodeID(t *testing.T) {
	ns, client := testutil.StartTestNATS(t)

	cfg := &Config{
		NatsHost: ns.ClientURL(),
		BaseDir:  t.TempDir(),
		NodeName: "node-a",
	}
	go func() { _ = launchService(cfg) }()

	publishSubject, err := ebsprovider.PublishSubject(cfg.NodeName)
	require.NoError(t, err)

	reqBody, err := json.Marshal(ebsprovider.PublishVolumeRequest{
		Versioned: ebsprovider.NewVersioned(),
		VolumeID:  "vol-does-not-exist",
		NodeID:    cfg.NodeName,
	})
	require.NoError(t, err)

	var msg *nats.Msg
	require.Eventually(t, func() bool {
		m, reqErr := client.Request(publishSubject, reqBody, time.Second)
		if reqErr != nil {
			return false
		}
		msg = m
		return true
	}, 5*time.Second, 50*time.Millisecond, "qemunbdd never answered %s", publishSubject)

	var resp ebsprovider.PublishVolumeResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	// The volume does not exist, so the request fails, but a reply at all
	// proves the node-scoped subject (not the wildcard) is what's subscribed.
	require.NotNil(t, resp.Error)
	assert.Equal(t, ebsprovider.ErrorCodeNotFound, resp.Error.Code)
}
