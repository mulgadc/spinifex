//go:build lambdatestlink

package testlink

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	lambdav1 "github.com/mulgadc/spinifex/contracts/lambda/v1"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/require"
)

type fakeIPs struct {
	allocErr, releaseErr error
	releaseFails         int
	allocated, released  []string
}

func (f *fakeIPs) Allocate(id string) (string, error) {
	f.allocated = append(f.allocated, id)
	return "10.15.8.250", f.allocErr
}
func (f *fakeIPs) ReleaseChecked(id string) error {
	f.released = append(f.released, id)
	if f.releaseFails > 0 {
		f.releaseFails--
		return errors.New("kv unavailable")
	}
	return f.releaseErr
}

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
	image       *Image
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
	elfLib := elfHeader(elfMachine[runtime.GOARCH])
	require.NoError(t, os.WriteFile(lib, elfLib, 0o600))
	sum := sha256.Sum256(elfLib)
	return &granted{store: store, att: att, lib: lib, libSHA: hex.EncodeToString(sum[:]), image: preparedImage(t)}
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
	d.Image = g.image
	return d
}

func baseRequest(url string, code []byte) Request {
	sum := sha256.Sum256(code)
	return Request{
		AttemptID: "0123456789ab", RequestID: "r", Region: "ap-southeast-2", AccountID: "123456789012",
		FunctionID: "fn1", RevisionID: "rev1", Handler: "bootstrap.handler", TimeoutMs: 3000, CodeURL: url, CodeSize: int64(len(code)),
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
			rep := g.daemon(&Daemon{IPs: ips, Taps: taps, HTTP: client, WorkDir: t.TempDir()}).Handle(context.Background(), tc.data(g), testCred)
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
			rep := g.daemon(&Daemon{IPs: ips, Taps: &fakeTaps{}, HTTP: srv.Client(), WorkDir: t.TempDir()}).Handle(context.Background(), data, testCred)
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
	require.Equal(t, OutcomeRefused, d.Handle(context.Background(), g.request(srv.URL, code), testCred).Outcome)
	require.Equal(t, execution.StateEnded, g.state(t))

	ips.allocated = nil
	rep := d.Handle(context.Background(), g.request(srv.URL, code), testCred)
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Empty(t, ips.allocated, "an ended grant never starts again")
}

func TestDaemon_TapNotCreatedRemovesNothingAndResolves(t *testing.T) {
	code := []byte("zip bytes")
	srv := codeServer(t, code)
	g := grant(t, code)
	ips, taps := &fakeIPs{}, &fakeTaps{createErr: fmt.Errorf("%w: %w", host.ErrTapNotCreated, host.ErrTapExists)}
	d := g.daemon(&Daemon{IPs: ips, Taps: taps, HTTP: srv.Client(), WorkDir: t.TempDir()})
	rep := d.Handle(context.Background(), g.request(srv.URL, code), testCred)
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
	d := g.daemon(&Daemon{IPs: ips, Taps: taps, HTTP: srv.Client(), WorkDir: work, HostIP: "127.0.0.1"})
	// A file where the attempt directory goes fails preparation after the tap exists.
	require.NoError(t, os.WriteFile(filepath.Join(work, "0123456789ab"), nil, 0o600))
	rep := d.Handle(context.Background(), g.request(srv.URL, code), testCred)
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Len(t, taps.removed, 1)
	require.Empty(t, ips.released)
	require.True(t, d.busy.Load())
	require.FileExists(t, filepath.Join(work, "0123456789ab"+markerSuffix))
	require.Equal(t, execution.StateAmbiguous, g.state(t))

	fresh := g.daemon(&Daemon{IPs: &fakeIPs{}, Taps: &fakeTaps{}, HTTP: srv.Client(), WorkDir: work})
	rep = fresh.Handle(context.Background(), g.request(srv.URL, code), testCred)
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
			rep := d.Handle(context.Background(), g.request(srv.URL, code), testCred)
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
	rep := d.Handle(context.Background(), g.request(srv.URL, code), testCred)
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
	rep := d.Handle(context.Background(), g.request("https://h/x", code), testCred)
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Contains(t, rep.Error, "another")
	require.Equal(t, execution.StateGranted, g.state(t))
}

func TestCheckRequest_RefusesGrantWithoutEnvironmentGeneration(t *testing.T) {
	cases := map[string]func(*execution.Grant){
		"no environment":       func(g *execution.Grant) { g.EnvironmentID = "" },
		"generation zero":      func(g *execution.Grant) { g.Generation = 0 },
		"slash in environment": func(g *execution.Grant) { g.EnvironmentID = "env/1" },
		"newline in grant":     func(g *execution.Grant) { g.GrantID = "g\n1" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := baseRequest("https://example.invalid/code", []byte("zip"))
			req.Grant = execution.Grant{GrantID: "g1", AttemptID: req.AttemptID, EnvironmentID: "env-1", Generation: 3, Node: testNode}
			require.NoError(t, CheckRequest(req))
			mutate(&req.Grant)
			require.ErrorContains(t, CheckRequest(req), "grant")
		})
	}
}

// The guest's certificates name the granted environment generation, and the
// grant entry it binds its run to names the same grant.
func TestPrepare_ScopeAndGrantEntryComeFromTheGrant(t *testing.T) {
	req := baseRequest("https://example.invalid/code", []byte("zip"))
	req.Grant = execution.Grant{GrantID: "g-7", AttemptID: req.AttemptID, EnvironmentID: "env-x", Generation: 3, Node: testNode}
	d := &Daemon{HostIP: "127.0.0.1"}
	dir := filepath.Join(t.TempDir(), "attempt")
	_, gen, ln, err := d.prepare(req, []byte("zip"), dir, "tap0", "02:00:00:00:00:01", "10.0.0.2")
	require.NoError(t, err)
	defer ln.Close()

	entry, err := os.ReadFile(filepath.Join(dir, "fwcfg-test-grant"))
	require.NoError(t, err)
	require.Equal(t, "g-7\n0123456789ab\nenv-x\n3\n", string(entry))
	handler, err := os.ReadFile(filepath.Join(dir, "fwcfg-test-handler"))
	require.NoError(t, err)
	require.Equal(t, req.Handler, string(handler))
	expect := gen.GuestEntries(ln.Addr().String())[lambdav1.EntryScope]
	require.Equal(t, "ap-southeast-2/123456789012/fn1/env-x/3", strings.TrimSpace(string(expect)))
}

// elfHeader is a bare 64-bit little-endian shared-object header for machine.
func elfHeader(machine elf.Machine) []byte {
	h := make([]byte, 64)
	copy(h, elf.ELFMAG)
	h[elf.EI_CLASS], h[elf.EI_DATA], h[elf.EI_VERSION] = byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(h[16:], uint16(elf.ET_DYN))
	binary.LittleEndian.PutUint16(h[18:], uint16(machine))
	binary.LittleEndian.PutUint32(h[20:], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint16(h[52:], 64)
	return h
}

func TestSameArch_RefusesOtherMachinesAndNonELF(t *testing.T) {
	require.NoError(t, sameArch(elfHeader(elfMachine[runtime.GOARCH])))
	other := elf.EM_AARCH64
	if runtime.GOARCH == "arm64" {
		other = elf.EM_X86_64
	}
	require.ErrorContains(t, sameArch(elfHeader(other)), "built for")
	require.ErrorContains(t, sameArch([]byte("library")), "not an ELF object")
}

// stepBoot makes the first CLOCK_BOOTTIME read return base and every later
// read return later, so the fixed deadline is set before the jump.
func stepBoot(t *testing.T, base, later time.Duration) {
	t.Helper()
	orig := bootNow
	var calls atomic.Int32
	bootNow = func() (time.Duration, error) {
		if calls.Add(1) == 1 {
			return base, nil
		}
		return later, nil
	}
	t.Cleanup(func() { bootNow = orig })
}

// A launch delayed past the deadline fixed before the grant started is refused
// before QEMU starts, and everything it took is released.
func TestDaemon_DelayedLaunchPastTheFixedDeadlineIsRefused(t *testing.T) {
	code := []byte("zip bytes")
	srv := codeServer(t, code)
	g := grant(t, code)
	ips, taps := &fakeIPs{}, &fakeTaps{}
	d := g.daemon(&Daemon{IPs: ips, Taps: taps, HTTP: srv.Client(), WorkDir: t.TempDir()})
	req := g.request(srv.URL, code)
	var parsed Request
	require.NoError(t, json.Unmarshal(req, &parsed))
	stepBoot(t, time.Hour, hardDeadlineFrom(time.Hour, parsed))

	rep := d.Handle(context.Background(), req, testCred)
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Equal(t, ErrDeadlineExpired.Error(), rep.Error)
	require.Equal(t, []string{"lambda-0123456789ab"}, ips.released)
	require.Len(t, taps.removed, 1)
	require.False(t, d.busy.Load())
	require.Equal(t, execution.StateEnded, g.state(t))
}

// A code fetch that uses its whole allowance still leaves the function its
// full configured timeout, plus the kill margin, before the fixed backstop.
func TestHardDeadline_SlowFetchKeepsTheFullFunctionTimeout(t *testing.T) {
	req := Request{TimeoutMs: 3000}
	base := time.Hour
	afterFetch := base + fetchTimeout
	stepBoot(t, base, afterFetch)

	boot, err := bootNow()
	require.NoError(t, err)
	deadline := hardDeadlineFrom(boot, req)

	require.False(t, expired(deadline), "a full-allowance fetch must not expire the attempt")
	require.Equal(t, timeoutFor(req)+HardKillMargin, deadline-afterFetch)
}

// The handler becomes the guest's _HANDLER, so it must be one AWS-shaped token.
func TestCheckRequest_RefusesUnusableHandler(t *testing.T) {
	for name, h := range map[string]string{
		"empty": "", "space": "a b", "newline": "a\nb", "nul": "a\x00b", "129 characters": strings.Repeat("h", 129),
	} {
		t.Run(name, func(t *testing.T) {
			req := baseRequest("https://example.invalid/code", []byte("zip"))
			req.Grant = execution.Grant{GrantID: "g1", AttemptID: req.AttemptID, EnvironmentID: "env-1", Generation: 3, Node: testNode}
			req.Handler = strings.Repeat("h", 128)
			require.NoError(t, CheckRequest(req))
			req.Handler = h
			require.ErrorContains(t, CheckRequest(req), "handler")
		})
	}
}

// unconfirmedStart lets the owner write the start, then reports the outcome
// as unknown, as a lost acknowledgement would.
type unconfirmedStart struct{ *execution.Store }

func (u unconfirmedStart) Start(ctx context.Context, g execution.Grant, want execution.Identity) error {
	if err := u.Store.Start(ctx, g, want); err != nil {
		return err
	}
	return execution.ErrOutcomeUnknown
}

func TestDaemon_UnknownStartOutcomeHoldsTheSlot(t *testing.T) {
	code := []byte("x")
	g := grant(t, code)
	ips, taps := &fakeIPs{}, &fakeTaps{}
	d := g.daemon(&Daemon{IPs: ips, Taps: taps, WorkDir: t.TempDir()})
	d.Grants = unconfirmedStart{g.store}

	rep := d.Handle(context.Background(), g.request("https://h/x", code), testCred)
	require.Equal(t, OutcomeUnknown, rep.Outcome)
	require.True(t, d.busy.Load(), "an unconfirmed start may have taken the node")
	require.Empty(t, ips.allocated)
	require.Equal(t, execution.StateRunning, g.state(t))

	rep = d.Handle(context.Background(), g.request("https://h/x", code), testCred)
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Contains(t, rep.Error, "holds this node's slot")
}
