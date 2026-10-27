//go:build e2e

package multinode

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// xfrmTwoSAs is `ip -s xfrm state list src A dst B proto esp` during a rekey,
// when the old and new SA to one peer are both installed.
const xfrmTwoSAs = `src 192.0.2.1 dst 192.0.2.2
	proto esp spi 0x00001001(4097) reqid 1(0x00000001) mode transport
	replay-window 0 seq 0x00000000 flag  (0x00000000)
	aead rfc4106(gcm(aes)) 0x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c (224 bits) 128
	lastused 2026-10-28 08:28:41
	anti-replay context: seq 0x0, oseq 0x3, bitmap 0x00000000
	sel src 192.0.2.1/32 dst 192.0.2.2/32 uid 0
	lifetime config:
	  limit: soft (INF)(bytes), hard (INF)(bytes)
	  limit: soft (INF)(packets), hard (INF)(packets)
	  expire add: soft 0(sec), hard 0(sec)
	  expire use: soft 0(sec), hard 0(sec)
	lifetime current:
	  33(bytes), 3(packets)
	  add 2026-10-28 08:28:41 use 2026-10-28 08:28:41
	stats:
	  replay-window 0 replay 0 failed 0
src 192.0.2.1 dst 192.0.2.2
	proto esp spi 0x00001003(4099) reqid 1(0x00000001) mode transport
	replay-window 0 seq 0x00000000 flag  (0x00000000)
	aead rfc4106(gcm(aes)) 0x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c (224 bits) 128
	sel src 192.0.2.1/32 dst 192.0.2.2/32 uid 0
	lifetime config:
	  limit: soft 1000(bytes), hard 2000(bytes)
	  limit: soft (INF)(packets), hard (INF)(packets)
	lifetime current:
	  1200450(bytes), 1100(packets)
	  add 2026-10-28 08:28:41 use 2026-10-28 08:28:41
`

func TestParseXfrmCounters(t *testing.T) {
	sas, err := parseXfrmCounters(xfrmTwoSAs)
	require.NoError(t, err)
	assert.Equal(t, map[string]saCounters{
		"0x00001001(4097)": {bytes: 33, packets: 3},
		"0x00001003(4099)": {bytes: 1200450, packets: 1100},
	}, sas)

	empty, err := parseXfrmCounters("")
	require.NoError(t, err)
	assert.Empty(t, empty, "no SA to the peer must read as none, not as zero traffic")

	_, err = parseXfrmCounters("\tlifetime current:\n\t  1(bytes), 1(packets)\n")
	assert.Error(t, err, "counters with no SPI cannot be attributed to an SA")

	_, err = parseXfrmCounters("\tproto esp spi 0x1 reqid 1 mode transport\n\tlifetime current:\n\t  garbage\n")
	assert.Error(t, err, "an unreadable counter line must fail rather than read as zero")
}

func TestSADelta(t *testing.T) {
	before := map[string]saCounters{
		"old":    {bytes: 5000, packets: 5},
		"steady": {bytes: 100, packets: 1},
	}
	after := map[string]saCounters{
		"steady": {bytes: 300100, packets: 301},
		"new":    {bytes: 50, packets: 2},
	}
	bytes, packets := saDelta(before, after)
	assert.Equal(t, uint64(300050), bytes, "an expired SA must not subtract and a new SA counts in full")
	assert.Equal(t, uint64(302), packets)
}

func TestClassifyUnderlay(t *testing.T) {
	out := `tcpdump: verbose output suppressed, use -v[v]... for full protocol decode
listening on va, link-type EN10MB (Ethernet), snapshot length 262144 bytes
IP 192.0.2.1 > 192.0.2.2: ESP(spi=0x00001001,seq=0x1), length 48
IP 192.0.2.1 > 192.0.2.2: ESP(spi=0x00001001,seq=0x2), length 48
IP 192.0.2.1.4500 > 192.0.2.2.4500: UDP-encap: ESP(spi=0x00001001,seq=0x1), length 48
IP 192.0.2.1.51771 > 192.0.2.2.4500:  [|isakmp_rfc3948]
IP 192.0.2.1.40317 > 192.0.2.2.6081: Geneve [length 6 < 8] (invalid)
IP 192.0.2.1.6081 > 192.0.2.2.39000: UDP, length 120

5 packets captured
5 packets received by filter
0 packets dropped by kernel
`
	assert.Equal(t, underlayCapture{esp: 3, geneve: 2, other: 1}, classifyUnderlay(out))
	assert.Equal(t, underlayCapture{}, classifyUnderlay("0 packets captured\n"))
}

func TestRouteDev(t *testing.T) {
	dev, ok := routeDev("192.0.2.2 dev br-ex src 192.0.2.1 uid 1000 \n    cache \n")
	assert.True(t, ok)
	assert.Equal(t, "br-ex", dev)

	_, ok = routeDev("RTNETLINK answers: Network is unreachable\n")
	assert.False(t, ok)
}
