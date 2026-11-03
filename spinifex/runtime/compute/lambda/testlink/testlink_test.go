//go:build lambdatestlink

package testlink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/require"
)

type fakeIPs struct {
	allocErr            error
	allocated, released []string
}

func (f *fakeIPs) Allocate(id string) (string, error) {
	f.allocated = append(f.allocated, id)
	return "10.15.8.250", f.allocErr
}
func (f *fakeIPs) Release(id string) { f.released = append(f.released, id) }

type fakeTaps struct {
	createErr, removeErr error
	created              host.OwnedTap
	removed              []host.OwnedTap
}

func (f *fakeTaps) CreateOwnedTap(spec vm.TapSpec, owner string) (host.OwnedTap, error) {
	if f.createErr != nil {
		return host.OwnedTap{}, f.createErr
	}
	f.created = host.OwnedTap{Name: spec.Name, IfIndex: 41, Owner: owner}
	return f.created, nil
}

func (f *fakeTaps) RemoveOwnedTap(tap host.OwnedTap) error {
	f.removed = append(f.removed, tap)
	return f.removeErr
}

func codeServer(t *testing.T, body []byte) *httptest.Server {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	t.Cleanup(srv.Close)
	return srv
}

const testNode = "node1"

// granted is a real owner holding one issued grant for the test request.
type granted struct {
	store       *execution.Store
	att         execution.Attempt
	lib, libSHA string
}

func grant(t *testing.T, code []byte) *granted {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	store, err := execution.Open(context.Background(), testutil.NewJetStream(t, nc))
	require.NoError(t, err)
	att, err := store.Issue(context.Background(), execution.Attempt{
		Grant:    execution.Grant{AttemptID: "0123456789ab", EnvironmentID: "env-0123456789ab", Generation: 1, Node: testNode},
		Identity: baseRequest("", code).Identity(),
		LaunchBy: time.Now().Add(time.Minute),
	})
	require.NoError(t, err)
	lib := filepath.Join(t.TempDir(), "hardkill.so")
	require.NoError(t, os.WriteFile(lib, []byte("library"), 0o600))
	sum := sha256.Sum256([]byte("library"))
	return &granted{store: store, att: att, lib: lib, libSHA: hex.EncodeToString(sum[:])}
}

func (g *granted) state(t *testing.T) string {
	t.Helper()
	a, err := g.store.Current(context.Background(), testNode)
	require.NoError(t, err)
	return a.State
}

// daemon wires the owner and a valid pinned initializer into d.
func (g *granted) daemon(d *Daemon) *Daemon {
	d.Node, d.Grants = testNode, g.store
	d.Initializer, d.InitializerSHA256 = g.lib, g.libSHA
	return d
}

func baseRequest(url string, code []byte) Request {
	sum := sha256.Sum256(code)
	return Request{
		AttemptID: "0123456789ab", RequestID: "r", Region: "ap-southeast-2", AccountID: "123456789012",
		FunctionID: "fn1", RevisionID: "rev1", TimeoutMs: 3000, CodeURL: url, CodeSize: int64(len(code)),
		CodeSHA256: hex.EncodeToString(sum[:]),
	}
}

func (g *granted) request(url string, code []byte) []byte {
	req := baseRequest(url, code)
	req.Grant = g.att.Grant
	b, _ := json.Marshal(req)
	return b
}

func TestDaemon_RefusesBeforeAllocating(t *testing.T) {
	code := []byte("zip bytes")
	good := codeServer(t, code)
	wrong := codeServer(t, []byte("other one"))
	short := codeServer(t, code[:4])
	cases := map[string]struct {
		data  func(g *granted) []byte
		state string
	}{
		// Refused before Start: the grant stays unstarted for the gateway to withdraw.
		"plain HTTP":     {func(g *granted) []byte { return g.request(strings.Replace(good.URL, "https", "http", 1), code) }, execution.StateGranted},
		"bad attempt ID": {func(*granted) []byte { return []byte(`{"attempt_id":"../x"}`) }, execution.StateGranted},
		"no grant": {func(*granted) []byte {
			b, _ := json.Marshal(baseRequest(good.URL, code))
			return b
		}, execution.StateGranted},
		// Refused after Start with nothing launched: the daemon records the end.
		"digest mismatch": {func(g *granted) []byte { return g.request(wrong.URL, code) }, execution.StateEnded},
		"size mismatch":   {func(g *granted) []byte { return g.request(short.URL, code) }, execution.StateEnded},
		"untrusted TLS":   {func(g *granted) []byte { return g.request(good.URL, code) }, execution.StateEnded},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := grant(t, code)
			ips, taps := &fakeIPs{}, &fakeTaps{}
			client := good.Client()
			if name == "untrusted TLS" {
				client = &http.Client{}
			}
			rep := g.daemon(&Daemon{IPs: ips, Taps: taps, HTTP: client, WorkDir: t.TempDir()}).Handle(context.Background(), tc.data(g))
			require.Equal(t, OutcomeRefused, rep.Outcome, rep.Error)
			require.Empty(t, ips.allocated)
			require.NotContains(t, rep.Error, good.URL)
			require.Equal(t, tc.state, g.state(t))
		})
	}
}

// The owner refuses a stale, replayed or mismatched grant before the daemon
// fetches or allocates anything, and the grant is left as it was.
func TestDaemon_OwnerRefusalComesBeforeAnyAllocation(t *testing.T) {
	code := []byte("zip bytes")
	cases := map[string]func(req *Request){
		"stale grant ID":    func(req *Request) { req.Grant.GrantID = "superseded" },
		"older sequence":    func(req *Request) { req.Grant.Seq = 0 },
		"other revision":    func(req *Request) { req.RevisionID = "rev2" },
		"other code digest": func(req *Request) { req.CodeSHA256 = strings.Repeat("0", 64) },
		"other request":     func(req *Request) { req.RequestID = "r2" },
		"other node":        func(req *Request) { req.Grant.Node = "node2" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fetched := false
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fetched = true
				_, _ = w.Write(code)
			}))
			t.Cleanup(srv.Close)
			g := grant(t, code)
			req := baseRequest(srv.URL, code)
			req.Grant = g.att.Grant
			mutate(&req)
			data, _ := json.Marshal(req)
			ips := &fakeIPs{}
			rep := g.daemon(&Daemon{IPs: ips, Taps: &fakeTaps{}, HTTP: srv.Client(), WorkDir: t.TempDir()}).Handle(context.Background(), data)
			require.Equal(t, OutcomeRefused, rep.Outcome)
			require.False(t, fetched)
			require.Empty(t, ips.allocated)
			require.Equal(t, execution.StateGranted, g.state(t))
		})
	}
}

func TestDaemon_ReplayedGrantIsRefused(t *testing.T) {
	code := []byte("zip bytes")
	srv := codeServer(t, code)
	g := grant(t, code)
	ips := &fakeIPs{}
	taps := &fakeTaps{createErr: fmt.Errorf("%w: %w", host.ErrTapNotCreated, host.ErrTapExists)}
	d := g.daemon(&Daemon{IPs: ips, Taps: taps, HTTP: srv.Client(), WorkDir: t.TempDir()})
	require.Equal(t, OutcomeRefused, d.Handle(context.Background(), g.request(srv.URL, code)).Outcome)
	require.Equal(t, execution.StateEnded, g.state(t))

	ips.allocated = nil
	rep := d.Handle(context.Background(), g.request(srv.URL, code))
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Empty(t, ips.allocated, "an ended grant never starts again")
}

func TestDaemon_TapNotCreatedRemovesNothingAndResolves(t *testing.T) {
	code := []byte("zip bytes")
	srv := codeServer(t, code)
	g := grant(t, code)
	ips, taps := &fakeIPs{}, &fakeTaps{createErr: fmt.Errorf("%w: %w", host.ErrTapNotCreated, host.ErrTapExists)}
	d := g.daemon(&Daemon{IPs: ips, Taps: taps, HTTP: srv.Client(), WorkDir: t.TempDir()})
	rep := d.Handle(context.Background(), g.request(srv.URL, code))
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Empty(t, taps.removed, "a tap this attempt did not create is never removed")
	require.Equal(t, []string{"lambda-0123456789ab"}, ips.released)
	id, err := d.unresolved()
	require.NoError(t, err)
	require.Empty(t, id)
	require.False(t, d.busy.Load())
	require.Equal(t, execution.StateEnded, g.state(t))
}

// A tap that will not go keeps the address, the marker, the slot and the
// grant, and a fresh daemon on the same work directory still refuses.
func TestDaemon_FailedCleanupHoldsTheSlotAcrossRestart(t *testing.T) {
	code := []byte("zip bytes")
	srv := codeServer(t, code)
	g := grant(t, code)
	work := t.TempDir()
	ips, taps := &fakeIPs{}, &fakeTaps{removeErr: errors.New("busy")}
	d := g.daemon(&Daemon{IPs: ips, Taps: taps, HTTP: srv.Client(), WorkDir: work, ImageDir: t.TempDir(), HostIP: "127.0.0.1"})
	// A file where the attempt directory goes fails preparation after the tap exists.
	require.NoError(t, os.WriteFile(filepath.Join(work, "0123456789ab"), nil, 0o600))
	rep := d.Handle(context.Background(), g.request(srv.URL, code))
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Len(t, taps.removed, 1)
	require.Empty(t, ips.released)
	require.True(t, d.busy.Load())
	require.FileExists(t, filepath.Join(work, "0123456789ab"+markerSuffix))
	require.Equal(t, execution.StateAmbiguous, g.state(t))

	fresh := g.daemon(&Daemon{IPs: &fakeIPs{}, Taps: &fakeTaps{}, HTTP: srv.Client(), WorkDir: work})
	rep = fresh.Handle(context.Background(), g.request(srv.URL, code))
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Contains(t, rep.Error, "unresolved")
	require.Equal(t, execution.StateAmbiguous, g.state(t))
}

// Neither failure proves nothing was left behind, so both keep the marker,
// the address, the slot and the grant.
func TestDaemon_UnprovenOwnershipHoldsTheAttempt(t *testing.T) {
	cases := map[string]struct {
		ips  *fakeIPs
		taps *fakeTaps
	}{
		"allocation outcome unknown": {ips: &fakeIPs{allocErr: errors.New("CAS timeout")}, taps: &fakeTaps{}},
		"tap may exist":              {ips: &fakeIPs{}, taps: &fakeTaps{createErr: errors.New("ifindex unreadable")}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code := []byte("zip bytes")
			srv := codeServer(t, code)
			g := grant(t, code)
			work := t.TempDir()
			d := g.daemon(&Daemon{IPs: tc.ips, Taps: tc.taps, HTTP: srv.Client(), WorkDir: work})
			rep := d.Handle(context.Background(), g.request(srv.URL, code))
			require.Equal(t, OutcomeRefused, rep.Outcome)
			require.Empty(t, tc.ips.released)
			require.Empty(t, tc.taps.removed)
			require.True(t, d.busy.Load())
			require.FileExists(t, filepath.Join(work, "0123456789ab"+markerSuffix))
			require.Equal(t, execution.StateAmbiguous, g.state(t))
		})
	}
}

func TestDaemon_MarkerBlocksBeforeAnyAllocation(t *testing.T) {
	code := []byte("zip bytes")
	srv := codeServer(t, code)
	g := grant(t, code)
	work := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(work, "aaaaaaaaaaaa"+markerSuffix), nil, 0o600))
	ips := &fakeIPs{}
	d := g.daemon(&Daemon{IPs: ips, Taps: &fakeTaps{}, HTTP: srv.Client(), WorkDir: work})
	rep := d.Handle(context.Background(), g.request(srv.URL, code))
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Empty(t, ips.allocated)
	require.False(t, d.busy.Load(), "a refused request takes no slot of its own")
	require.Equal(t, execution.StateGranted, g.state(t))
}

func TestDaemon_OneAttemptAtATime(t *testing.T) {
	code := []byte("x")
	g := grant(t, code)
	d := g.daemon(&Daemon{})
	d.busy.Store(true)
	rep := d.Handle(context.Background(), g.request("https://h/x", code))
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Contains(t, rep.Error, "another")
	require.Equal(t, execution.StateGranted, g.state(t))
}
