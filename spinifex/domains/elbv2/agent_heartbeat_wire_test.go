package elbv2

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ingress/aws/query"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLBAgentHeartbeatInput_OldAgentDecodesUnchanged(t *testing.T) {
	t.Parallel()
	// Exactly the parameters an agent without the data-path probe sends.
	params := map[string]string{
		"Action":                   "LBAgentHeartbeat",
		"Version":                  "2015-12-01",
		"LBID":                     "lb-abc123",
		"Servers.member.1.Backend": "bk_tg1",
		"Servers.member.1.Server":  "srv_i-web1",
		"Servers.member.1.Status":  "UP",
		"Servers.member.2.Backend": "bk_tg1",
		"Servers.member.2.Server":  "srv_i-web2",
		"Servers.member.2.Status":  "DOWN",
	}

	var in LBAgentHeartbeatInput
	require.NoError(t, query.QueryParamsToStruct(params, &in))

	require.NotNil(t, in.LBID)
	assert.Equal(t, "lb-abc123", *in.LBID)
	require.Len(t, in.Servers, 2)
	assert.Equal(t, "DOWN", *in.Servers[1].Status)
	assert.Nil(t, in.InstanceID, "an old agent reports no instance ID")
	assert.Empty(t, in.NICs, "an old agent reports no NICs")
	assert.Len(t, in.toHealthReport().Servers, 2)
}

func TestLBAgentHeartbeatInput_DecodesDataPathEvidence(t *testing.T) {
	t.Parallel()
	params := map[string]string{
		"LBID":                              "lb-abc123",
		"InstanceID":                        "i-0123456789abcdef0",
		"NICs.member.1.MAC":                 "02:00:00:aa:00:01",
		"NICs.member.1.ProbeOK":             "false",
		"NICs.member.1.ConsecutiveFailures": "4",
		"NICs.member.1.RxPackets":           "100",
		"NICs.member.1.TxPackets":           "200",
		"NICs.member.1.RxDropped":           "3",
		"NICs.member.1.TxDropped":           "9",
		"NICs.member.2.MAC":                 "02:00:00:aa:00:03",
		"NICs.member.2.ProbeOK":             "true",
		"NICs.member.2.ConsecutiveFailures": "0",
	}

	var in LBAgentHeartbeatInput
	require.NoError(t, query.QueryParamsToStruct(params, &in))

	require.NotNil(t, in.InstanceID)
	assert.Equal(t, "i-0123456789abcdef0", *in.InstanceID)
	require.Len(t, in.NICs, 2)

	n := in.NICs[0]
	assert.Equal(t, "02:00:00:aa:00:01", *n.MAC)
	assert.False(t, *n.ProbeOK)
	assert.EqualValues(t, 4, *n.ConsecutiveFailures)
	assert.EqualValues(t, 100, *n.RxPackets)
	assert.EqualValues(t, 200, *n.TxPackets)
	assert.EqualValues(t, 3, *n.RxDropped)
	assert.EqualValues(t, 9, *n.TxDropped)

	assert.True(t, *in.NICs[1].ProbeOK)
	assert.Nil(t, in.NICs[1].RxPackets, "an omitted counter stays unset")
}
