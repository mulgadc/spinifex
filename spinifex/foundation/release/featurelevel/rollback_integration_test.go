package featurelevel

import (
	"encoding/json"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveGate answers node's status route from the node's real gate, the way a
// daemon's responder does.
func serveGate(t *testing.T, nc *nats.Conn, node string, g *Gate) {
	t.Helper()
	handle := g.ServeStatus(node, Build)
	sub, err := nc.Subscribe(StatusSubject(node), func(m *nats.Msg) {
		var req StatusRequest
		if json.Unmarshal(m.Data, &req) != nil {
			return
		}
		reply, err := handle(t.Context(), &req, "")
		if err != nil {
			_ = m.Respond(nil)
			return
		}
		out := nats.NewMsg("")
		out.Data, _ = json.Marshal(reply)
		out.Header.Set(natsmsg.NodeIDHeader, node)
		_ = m.RespondMsg(out)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// A request admitted before the rollback was recorded keeps the rollback from
// finishing until it is done, through the real gate, responder, probe and runner.
func TestRollback_AnAdmittedRequestBlocksTheFinishUntilItIsReleased(t *testing.T) {
	t.Parallel()
	_, nc, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "cluster-config-it", History: 1})
	require.NoError(t, err)
	store := NewStore(LeaderKV(js, kv))
	_, adv, err := store.Advance(t.Context(), members(1, true, true))
	require.NoError(t, err)
	require.True(t, adv)

	gate := NewGate(store)
	serveGate(t, nc, "n1", gate)
	serveGate(t, nc, "n2", NewGate(store))

	done, ok, err := gate.Admit(t.Context(), 1)
	require.NoError(t, err)
	require.True(t, ok)

	probe := NewProbe(nc, []string{"n1", "n2"})
	probe.wait, probe.grace = 400*time.Millisecond, 20*time.Millisecond
	d := RollbackDeps{
		Store: store, Probe: probe.Probe, Drains: DrainChecks, Build: Build,
		AckTimeout: 20 * time.Second, PollInterval: 50 * time.Millisecond,
	}

	result := make(chan error, 1)
	go func() {
		_, err := RunRollback(t.Context(), 0, d)
		result <- err
	}()

	// While the admitted request is held the rollback is recorded but unfinished.
	require.Eventually(t, func() bool {
		rec, _, rerr := store.Read(t.Context())
		return rerr == nil && rec.Rollback != nil
	}, 5*time.Second, 20*time.Millisecond)
	select {
	case err := <-result:
		t.Fatalf("rollback finished with an admitted request still held: %v", err)
	case <-time.After(600 * time.Millisecond):
	}
	rec, _, err := store.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, rec.Level)
	refusedDone, ok, err := gate.Admit(t.Context(), 1)
	refusedDone()
	require.NoError(t, err)
	assert.False(t, ok, "new admissions above the target are refused while the rollback is recorded")

	done()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("rollback did not finish after the admitted request was released")
	}
	rec, _, err = store.Read(t.Context())
	require.NoError(t, err)
	assert.Zero(t, rec.Level)
	assert.Nil(t, rec.Rollback)
}
