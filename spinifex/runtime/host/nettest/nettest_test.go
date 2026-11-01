package nettest

import (
	"testing"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Captured from iperf 2.2.1 with -P 3 -y C.
const threeStreams = `20261102023725,127.0.0.1,36493,127.0.0.1,5099,1,0.0-2.0,1544421440,6124843671
20261102023725,127.0.0.1,40019,127.0.0.1,5099,3,0.0-2.0,1603665984,6361182295
20261102023725,127.0.0.1,35431,127.0.0.1,5099,2,0.0-2.0,1424490560,5647792895
20261102023725,127.0.0.1,0,127.0.0.1,5099,-1,0.0-2.0,4572577984,18116331744
`

func TestIperfCSVTakesTheSumLine(t *testing.T) {
	var out clusterv1.NetTestRunResponse
	require.NoError(t, parseIperfCSV(threeStreams, &out))

	assert.InDelta(t, 18116331744, out.BitsPerSecond, 1)
	assert.Equal(t, int64(4572577984), out.Bytes)
	assert.InDelta(t, 2.0, out.Seconds, 0.001)
	assert.Equal(t, 3, out.Streams)
	assert.Len(t, out.StreamBPS, 3)
}

func TestIperfCSVWithOneStreamHasNoSumLine(t *testing.T) {
	var out clusterv1.NetTestRunResponse
	require.NoError(t, parseIperfCSV("20261102023725,10.0.0.1,36493,10.0.0.2,5001,3,0.0-10.0,1180000000,943718400\n", &out))

	assert.InDelta(t, 943718400, out.BitsPerSecond, 1)
	assert.Equal(t, 1, out.Streams)
}

func TestIperfCSVRefusesAnEmptyReport(t *testing.T) {
	var out clusterv1.NetTestRunResponse
	assert.Error(t, parseIperfCSV("connect failed: Connection refused\n", &out))
}

func TestPingSummary(t *testing.T) {
	minMs, avgMs, maxMs, loss := parsePing(`--- 10.2.0.3 ping statistics ---
10 packets transmitted, 9 received, 10% packet loss, time 1803ms
rtt min/avg/max/mdev = 0.041/0.063/0.120/0.021 ms
`)
	assert.InDelta(t, 0.041, minMs, 1e-9)
	assert.InDelta(t, 0.063, avgMs, 1e-9)
	assert.InDelta(t, 0.120, maxMs, 1e-9)
	assert.InDelta(t, 10, loss, 1e-9)
}
