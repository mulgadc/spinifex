package host

import (
	"context"
	"net/netip"
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
