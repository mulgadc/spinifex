package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// drainFixture is a two-node formation at the rollback revision, idle unless
// a test changes it.
type drainFixture struct {
	drain   *gatewayEndpointDrain
	rev     uint64
	live    int
	holding map[string]int
	proxies map[string]projection.EndpointProxyStatus
	silent  map[string]bool
	out     bytes.Buffer
}

func newDrainFixture(t *testing.T) *drainFixture {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "feature-level-test"})
	require.NoError(t, err)
	store := featurelevel.NewStore(kv)
	_, _, err = store.Advance(t.Context(), []featurelevel.Member{{Node: "n1", Level: 2, Current: true}, {Node: "n2", Level: 2, Current: true}})
	require.NoError(t, err)
	_, rev, err := store.StartRollback(t.Context(), 1, featurelevel.Build)
	require.NoError(t, err)

	f := &drainFixture{rev: rev, holding: map[string]int{}, silent: map[string]bool{}, proxies: map[string]projection.EndpointProxyStatus{
		"n1": {Node: "n1", Revision: rev}, "n2": {Node: "n2", Revision: rev},
	}}
	f.drain = &gatewayEndpointDrain{
		store: store, nodes: []string{"n1", "n2"}, out: &f.out,
		daemons: func(context.Context) []featurelevel.NodeStatus {
			var out []featurelevel.NodeStatus
			for _, n := range []string{"n1", "n2"} {
				if f.silent["daemon-"+n] {
					out = append(out, featurelevel.NodeStatus{Node: n, Reason: "no-reply"})
					continue
				}
				r := &featurelevel.StatusReply{Node: n, Revision: rev}
				if f.holding[n] > 0 {
					r.Holding = map[int]int{featurelevel.GatewayEndpoints: f.holding[n]}
				}
				out = append(out, featurelevel.NodeStatus{Node: n, Reply: r})
			}
			return out
		},
		proxy: func(_ context.Context, node string) (projection.EndpointProxyStatus, error) {
			if f.silent[node] {
				return projection.EndpointProxyStatus{}, errors.New("no reply")
			}
			return f.proxies[node], nil
		},
		live: func(context.Context) (int, error) { return f.live, nil },
	}
	return f
}

func TestGatewayEndpointDrain_IdleFormationDrains(t *testing.T) {
	f := newDrainFixture(t)
	n, err := f.drain.check(t.Context())
	require.NoError(t, err)
	assert.Zero(t, n)
}

// Each kind of dependent work holds the rollback on its own.
func TestGatewayEndpointDrain_CountsEveryDependent(t *testing.T) {
	cases := map[string]func(f *drainFixture){
		"a live endpoint": func(f *drainFixture) { f.live = 1 },
		"an open session": func(f *drainFixture) {
			f.proxies["n2"] = projection.EndpointProxyStatus{Node: "n2", Revision: f.rev, Sessions: 1}
		},
		"an endpoint a proxy still admits by": func(f *drainFixture) {
			f.proxies["n2"] = projection.EndpointProxyStatus{Node: "n2", Revision: f.rev, Endpoints: 1}
		},
		"a node's demux flows": func(f *drainFixture) { f.holding["n1"] = 1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDrainFixture(t)
			mutate(f)
			n, err := f.drain.check(t.Context())
			require.NoError(t, err)
			assert.Equal(t, 1, n)
		})
	}
}

// A node that cannot say it has read the rollback, or an endpoint count that
// cannot be read, is not idle.
func TestGatewayEndpointDrain_UnreadNodeFails(t *testing.T) {
	cases := map[string]func(f *drainFixture){
		"silent vpcd": func(f *drainFixture) { f.silent["n2"] = true },
		"stale vpcd": func(f *drainFixture) {
			f.proxies["n1"] = projection.EndpointProxyStatus{Node: "n1", Revision: f.rev - 1}
		},
		"silent daemon": func(f *drainFixture) { f.silent["daemon-n1"] = true },
		"unreadable endpoint bucket": func(f *drainFixture) {
			f.drain.live = func(context.Context) (int, error) { return 0, errors.New("bucket unavailable") }
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDrainFixture(t)
			mutate(f)
			_, err := f.drain.check(t.Context())
			require.Error(t, err)
		})
	}
}

func TestGatewayEndpointDrain_UnconnectedFails(t *testing.T) {
	_, err := (&gatewayEndpointDrain{}).check(t.Context())
	require.Error(t, err)
}

func TestRequestProxyStatus_RefusesAnotherNodesAnswer(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	answer := func(st projection.EndpointProxyStatus) {
		sub, err := nc.Subscribe(projection.EndpointProxyStatusSubject("n1"), func(m *nats.Msg) {
			data, _ := json.Marshal(st)
			_ = m.Respond(data)
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	ask := requestProxyStatus(nc)

	answer(projection.EndpointProxyStatus{Node: "n2", Revision: 9})
	_, err := ask(t.Context(), "n1")
	require.ErrorContains(t, err, "answered by")

	_, err = ask(t.Context(), "n1.>")
	require.ErrorContains(t, err, "invalid node")
}

func TestRequestProxyStatus_ReadsTheAnswer(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	sub, err := nc.Subscribe(projection.EndpointProxyStatusSubject("n1"), func(m *nats.Msg) {
		var req projection.EndpointProxyStatusRequest
		if json.Unmarshal(m.Data, &req) != nil || req.Node != "n1" {
			data, _ := json.Marshal(projection.EndpointProxyStatus{Error: "wrong node"})
			_ = m.Respond(data)
			return
		}
		data, _ := json.Marshal(projection.EndpointProxyStatus{Node: "n1", Revision: 9, Sessions: 2})
		_ = m.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	st, err := requestProxyStatus(nc)(t.Context(), "n1")
	require.NoError(t, err)
	assert.Equal(t, uint64(9), st.Revision)
	assert.Equal(t, 2, st.Sessions)
}
