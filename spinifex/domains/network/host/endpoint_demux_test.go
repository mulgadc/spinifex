package host

import (
	"context"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptRunner records every command and answers dump-flows from dump.
type scriptRunner struct {
	dump  string
	calls []string
}

var _ Runner = (*scriptRunner)(nil)

func (s *scriptRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	s.calls = append(s.calls, call)
	if strings.Contains(call, "dump-flows") {
		return []byte(s.dump), nil
	}
	return nil, nil
}

const demuxENI = "eni-0abc1234deadbeef"

// A pass adds every wanted flow, removes only flows for entries no longer
// wanted, and leaves the tap's IMDS flows alone.
func TestEnsureEndpointDemux_ReplacesTheEntrySet(t *testing.T) {
	endpoint := IMDSEndpointName(demuxENI)
	cookie := endpointFlowCookie(endpoint)
	s := &scriptRunner{dump: "NXST_FLOW reply (xid=0x4):\n" +
		" " + "cookie=" + cookie + ", priority=200,tcp,in_port=203,nw_dst=192.0.2.10,tp_dst=8443 actions=mod_dl_dst:02:00:00:00:01:fe,output:204\n" +
		" " + "cookie=" + cookie + ", priority=200,tcp,in_port=203,nw_dst=192.0.2.12,tp_dst=8443 actions=mod_dl_dst:02:00:00:00:01:fe,output:204\n"}

	require.NoError(t, EnsureEndpointDemux(context.Background(), s, demuxENI,
		[]EndpointEntry{entry("192.0.2.11:8443"), entry("192.0.2.10:8443")}))

	tap := "tap0abc1234dead"
	mac := IMDSEndpointMAC(demuxENI)
	assert.Equal(t, []string{
		"ovs-ofctl add-flow br-imds cookie=" + cookie + ",table=0,priority=200,in_port=" + tap +
			",tcp,nw_dst=192.0.2.10,tp_dst=8443,actions=mod_dl_dst:" + mac + ",output:" + endpoint,
		"ovs-ofctl add-flow br-imds cookie=" + cookie + ",table=0,priority=200,in_port=" + tap +
			",tcp,nw_dst=192.0.2.11,tp_dst=8443,actions=mod_dl_dst:" + mac + ",output:" + endpoint,
		"ovs-ofctl --no-stats dump-flows br-imds cookie=" + cookie + "/-1",
		"ovs-ofctl --strict del-flows br-imds cookie=" + cookie + "/-1,table=0,priority=200,in_port=" + tap +
			",tcp,nw_dst=192.0.2.12,tp_dst=8443",
	}, s.calls)
	for _, c := range s.calls {
		assert.NotContains(t, c, imdsFlowCookie(endpoint), "IMDS flows must be untouched")
	}
}

func TestEnsureEndpointDemux_EmptySetRemovesByCookie(t *testing.T) {
	s := &scriptRunner{}
	require.NoError(t, EnsureEndpointDemux(context.Background(), s, demuxENI, nil))
	assert.Equal(t, []string{
		"ovs-ofctl del-flows br-imds cookie=" + endpointFlowCookie(IMDSEndpointName(demuxENI)) + "/-1",
	}, s.calls)
}

func TestEnsureEndpointDemux_RefusesBadEntries(t *testing.T) {
	s := &scriptRunner{}
	err := EnsureEndpointDemux(context.Background(), s, demuxENI,
		[]EndpointEntry{entry("192.0.2.10:8443"), {Addr: netip.MustParseAddr("192.0.2.11")}})
	require.Error(t, err)
	assert.Empty(t, s.calls)
}

// The endpoint cookie is distinct from the IMDS one, so the IMDS drift count and
// re-attach, which work by the IMDS cookie, never touch endpoint flows.
func TestEndpointFlowCookieIsDistinctFromIMDS(t *testing.T) {
	endpoint := IMDSEndpointName(demuxENI)
	assert.NotEqual(t, imdsFlowCookie(endpoint), endpointFlowCookie(endpoint))
	assert.False(t, strings.HasPrefix(endpointFlowCookie(endpoint), imdsCookiePrefix))
}

// Tearing a tap down removes its endpoint flows as well as its IMDS flows, since
// OVS keeps a flow whose in_port has gone.
func TestRemoveTapDatapath_ClearsEndpointDemux(t *testing.T) {
	s := &scriptRunner{}
	d := testDatapath()
	require.NoError(t, RemoveTapDatapath(context.Background(), s, d))
	assert.Contains(t, s.calls, "ovs-ofctl del-flows br-imds cookie="+imdsFlowCookie(d.Endpoint)+"/-1")
	assert.Contains(t, s.calls, "ovs-ofctl del-flows br-imds cookie="+endpointFlowCookie(d.Endpoint)+"/-1")
}

func TestParseEndpointDemuxFlows(t *testing.T) {
	got, err := parseEndpointDemuxFlows([]byte("NXST_FLOW reply (xid=0x4):\n" +
		" cookie=0xa1e581c5cc1d, priority=200,tcp,in_port=203,nw_dst=192.0.2.10,tp_dst=8443 actions=mod_dl_dst:02:00:00:00:01:fe,output:204\n"))
	require.NoError(t, err)
	assert.Equal(t, []EndpointEntry{entry("192.0.2.10:8443")}, got)

	_, err = parseEndpointDemuxFlows([]byte(" cookie=0xa1e5, priority=200,tcp,in_port=203 actions=drop\n"))
	require.Error(t, err, "a flow under the cookie that names no destination is not silently skipped")
}

// One dump answers for every ENI, each with only its own tap's flows, an ENI
// whose flows are all gone is present with none, and the rest count as unowned.
func TestEndpointDemuxStates_GroupsByTap(t *testing.T) {
	const other = "eni-0fedcba987654321"
	const flushed = "eni-0123456789abcdef"
	mine := endpointFlowCookie(IMDSEndpointName(demuxENI))
	theirs := endpointFlowCookie(IMDSEndpointName(other))
	s := &scriptRunner{dump: "NXST_FLOW reply (xid=0x4):\n" +
		" cookie=" + mine + ", priority=200,tcp,in_port=203,nw_dst=192.0.2.10,tp_dst=8443 actions=output:204\n" +
		" cookie=" + theirs + ", priority=200,tcp,in_port=205,nw_dst=192.0.2.11,tp_dst=8443 actions=output:206\n" +
		" cookie=" + mine + ", priority=200,tcp,in_port=203,nw_dst=192.0.2.12,tp_dst=8443 actions=output:204\n"}

	got, unowned, err := EndpointDemuxStates(context.Background(), s, []string{demuxENI, flushed})
	require.NoError(t, err)
	theirsCookie, err := strconv.ParseUint(theirs, 0, 64)
	require.NoError(t, err)
	assert.Equal(t, []uint64{theirsCookie}, unowned, "the cookie of an ENI not asked for")
	others, _, err := EndpointDemuxStates(context.Background(), s, []string{other})
	require.NoError(t, err)
	assert.Equal(t, []EndpointEntry{entry("192.0.2.10:8443"), entry("192.0.2.12:8443")}, got[demuxENI])
	assert.Equal(t, []EndpointEntry{entry("192.0.2.11:8443")}, others[other])
	v, ok := got[flushed]
	assert.True(t, ok, "an ENI with no flows is read, not missing")
	assert.Empty(t, v)
	assert.Equal(t, "ovs-ofctl --no-stats dump-flows br-imds cookie=0xa1e500000000/0xffff00000000", s.calls[0])
}

type failingRunner struct{}

func (failingRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return []byte("connection refused"), assert.AnError
}

func TestEndpointDemuxStates_DumpFailureReadsNothing(t *testing.T) {
	got, _, err := EndpointDemuxStates(context.Background(), failingRunner{}, []string{demuxENI})
	require.Error(t, err)
	assert.Nil(t, got)
}

// An unowned cookie's flows are deleted by exactly that cookie, and a cookie
// outside the endpoint range, such as an IMDS flow's, is never touched.
func TestRemoveEndpointDemuxCookie_OnlyEndpointCookies(t *testing.T) {
	cookie := endpointFlowCookie(IMDSEndpointName(demuxENI))
	c, err := strconv.ParseUint(cookie, 0, 64)
	require.NoError(t, err)
	s := &scriptRunner{}
	require.NoError(t, RemoveEndpointDemuxCookie(context.Background(), s, c))
	assert.Equal(t, []string{"ovs-ofctl del-flows " + IMDSBridge + " cookie=" + cookie + "/-1"}, s.calls)

	s = &scriptRunner{}
	require.Error(t, RemoveEndpointDemuxCookie(context.Background(), s, 0x1a5d00000001))
	assert.Empty(t, s.calls)
}
