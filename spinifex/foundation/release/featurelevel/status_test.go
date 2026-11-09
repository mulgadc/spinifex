package featurelevel

import (
	"encoding/json"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serve answers node's status route with reply, naming headerNode in the reply
// header the way a daemon names itself.
func serve(t *testing.T, nc *nats.Conn, node, headerNode string, reply any) {
	t.Helper()
	sub, err := nc.Subscribe(StatusSubject(node), func(m *nats.Msg) {
		out := nats.NewMsg("")
		out.Data, _ = json.Marshal(reply)
		out.Header.Set(natsmsg.NodeIDHeader, headerNode)
		_ = m.RespondMsg(out)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func shortProbe(nc *nats.Conn, nodes ...string) *Probe {
	p := NewProbe(nc, nodes)
	p.wait, p.grace = 400*time.Millisecond, 20*time.Millisecond
	return p
}

func TestServeStatus_RefusesARequestNamingAnotherNode(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	h := NewGate(s).ServeStatus("n1", Build)
	_, err := h(t.Context(), &StatusRequest{Node: "n2"}, "")
	require.Error(t, err)
}

func TestServeStatus_RereadsTheRecordAndReportsInFlightPerLevel(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	g := NewGate(s)
	h := g.ServeStatus("n1", Build)

	first, err := h(t.Context(), &StatusRequest{Node: "n1"}, "")
	require.NoError(t, err)
	assert.Equal(t, "n1", first.Node)
	assert.Equal(t, Build, first.Build)
	assert.Zero(t, first.Revision, "no record yet")

	_, _, err = s.Advance(t.Context(), members(1, true, true))
	require.NoError(t, err)
	done, _, err := g.Admit(t.Context(), 1)
	require.NoError(t, err)
	second, err := h(t.Context(), &StatusRequest{Node: "n1"}, "")
	require.NoError(t, err)
	assert.Greater(t, second.Revision, first.Revision, "the reply reads the record afresh")
	assert.Equal(t, map[int]int{1: 1}, second.InFlight)
	assert.Equal(t, 1, second.InFlightTotal)
	done()
	third, err := h(t.Context(), &StatusRequest{Node: "n1"}, "")
	require.NoError(t, err)
	assert.Zero(t, third.InFlightTotal)
}

func TestProbe_ReportsEachNodeAndTheReasonForASilentOne(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	serve(t, nc, "n1", "n1", StatusReply{Node: "n1", Build: Build, Revision: 7})

	got := shortProbe(nc, "n1", "silent").Probe(t.Context())
	require.Len(t, got, 2)
	assert.Equal(t, "n1", got[0].Node)
	require.NotNil(t, got[0].Reply)
	assert.EqualValues(t, 7, got[0].Reply.Revision)
	assert.Equal(t, "silent", got[1].Node)
	assert.Nil(t, got[1].Reply)
	assert.Equal(t, "no-reply", got[1].Reason)
}

func TestProbe_RejectsAReplyNamingAnotherNode(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	serve(t, nc, "body", "body", StatusReply{Node: "other", Build: Build})
	serve(t, nc, "header", "other", StatusReply{Node: "header", Build: Build})

	got := shortProbe(nc, "body", "header").Probe(t.Context())
	assert.Nil(t, got[0].Reply, "a body naming another node is rejected")
	assert.Equal(t, "wrong-identity", got[0].Reason)
	assert.Nil(t, got[1].Reply, "a header naming another node is rejected")
	assert.Equal(t, "wrong-identity", got[1].Reason)
}

func TestProbe_TwoRespondersForOneNodeIsADuplicate(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	serve(t, nc, "n1", "n1", StatusReply{Node: "n1", Build: Build})
	serve(t, nc, "n1", "n1", StatusReply{Node: "n1", Build: Build})
	got := shortProbe(nc, "n1").Probe(t.Context())
	assert.Nil(t, got[0].Reply)
	assert.Equal(t, "duplicate", got[0].Reason)
}

func TestProbe_MalformedReplyAndInvalidNodeID(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	serve(t, nc, "n1", "n1", "not an object")
	got := shortProbe(nc, "n1", "a.b").Probe(t.Context())
	assert.Equal(t, "invalid-node-id", got[0].Reason, "sorted: a.b first")
	assert.Equal(t, "malformed", got[1].Reason)
}
