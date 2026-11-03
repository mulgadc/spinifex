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

func request(url string, code []byte) []byte {
	sum := sha256.Sum256(code)
	b, _ := json.Marshal(Request{
		AttemptID: "0123456789ab", RequestID: "r", Region: "ap-southeast-2", AccountID: "123456789012",
		FunctionID: "fn1", TimeoutMs: 3000, CodeURL: url, CodeSize: int64(len(code)), CodeSHA256: hex.EncodeToString(sum[:]),
	})
	return b
}

func TestDaemon_RefusesBeforeAllocating(t *testing.T) {
	code := []byte("zip bytes")
	good := codeServer(t, code)
	wrong := codeServer(t, []byte("other one"))
	short := codeServer(t, code[:4])
	cases := map[string][]byte{
		"plain HTTP":      request(strings.Replace(good.URL, "https", "http", 1), code),
		"bad attempt ID":  []byte(`{"attempt_id":"../x"}`),
		"digest mismatch": request(wrong.URL, code),
		"size mismatch":   request(short.URL, code),
		"untrusted TLS":   request(good.URL, code),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			ips, taps := &fakeIPs{}, &fakeTaps{}
			client := good.Client()
			if name == "untrusted TLS" {
				client = &http.Client{}
			}
			rep := (&Daemon{IPs: ips, Taps: taps, HTTP: client, WorkDir: t.TempDir()}).Handle(context.Background(), data)
			require.Equal(t, OutcomeRefused, rep.Outcome, rep.Error)
			require.Empty(t, ips.allocated)
			require.NotContains(t, rep.Error, "secret")
			require.NotContains(t, rep.Error, good.URL)
		})
	}
}

func TestDaemon_TapNotCreatedRemovesNothingAndResolves(t *testing.T) {
	code := []byte("zip bytes")
	srv := codeServer(t, code)
	ips, taps := &fakeIPs{}, &fakeTaps{createErr: fmt.Errorf("%w: %w", host.ErrTapNotCreated, host.ErrTapExists)}
	d := &Daemon{IPs: ips, Taps: taps, HTTP: srv.Client(), WorkDir: t.TempDir()}
	rep := d.Handle(context.Background(), request(srv.URL, code))
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Empty(t, taps.removed, "a tap this attempt did not create is never removed")
	require.Equal(t, []string{"lambda-0123456789ab"}, ips.released)
	id, err := d.unresolved()
	require.NoError(t, err)
	require.Empty(t, id)
	require.False(t, d.busy.Load())
}

// A tap that will not go keeps the address, the marker and the slot, and a
// fresh daemon on the same work directory still refuses.
func TestDaemon_FailedCleanupHoldsTheSlotAcrossRestart(t *testing.T) {
	code := []byte("zip bytes")
	srv := codeServer(t, code)
	work := t.TempDir()
	ips, taps := &fakeIPs{}, &fakeTaps{removeErr: errors.New("busy")}
	d := &Daemon{IPs: ips, Taps: taps, HTTP: srv.Client(), WorkDir: work, ImageDir: t.TempDir(), HostIP: "127.0.0.1"}
	// A file where the attempt directory goes fails preparation after the tap exists.
	require.NoError(t, os.WriteFile(filepath.Join(work, "0123456789ab"), nil, 0o600))
	rep := d.Handle(context.Background(), request(srv.URL, code))
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Len(t, taps.removed, 1)
	require.Empty(t, ips.released)
	require.True(t, d.busy.Load())
	require.FileExists(t, filepath.Join(work, "0123456789ab"+markerSuffix))

	fresh := &Daemon{IPs: &fakeIPs{}, Taps: &fakeTaps{}, HTTP: srv.Client(), WorkDir: work}
	rep = fresh.Handle(context.Background(), request(srv.URL, code))
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Contains(t, rep.Error, "unresolved")
}

// Neither failure proves nothing was left behind, so both keep the marker,
// the address and the slot.
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
			work := t.TempDir()
			d := &Daemon{IPs: tc.ips, Taps: tc.taps, HTTP: srv.Client(), WorkDir: work}
			rep := d.Handle(context.Background(), request(srv.URL, code))
			require.Equal(t, OutcomeRefused, rep.Outcome)
			require.Empty(t, tc.ips.released)
			require.Empty(t, tc.taps.removed)
			require.True(t, d.busy.Load())
			require.FileExists(t, filepath.Join(work, "0123456789ab"+markerSuffix))
		})
	}
}

func TestDaemon_MarkerBlocksBeforeAnyAllocation(t *testing.T) {
	code := []byte("zip bytes")
	srv := codeServer(t, code)
	work := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(work, "aaaaaaaaaaaa"+markerSuffix), nil, 0o600))
	ips := &fakeIPs{}
	d := &Daemon{IPs: ips, Taps: &fakeTaps{}, HTTP: srv.Client(), WorkDir: work}
	rep := d.Handle(context.Background(), request(srv.URL, code))
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Empty(t, ips.allocated)
	require.False(t, d.busy.Load(), "a refused request takes no slot of its own")
}

func TestDaemon_OneAttemptAtATime(t *testing.T) {
	d := &Daemon{}
	d.busy.Store(true)
	code := []byte("x")
	rep := d.Handle(context.Background(), request("https://h/x", code))
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Contains(t, rep.Error, "another")
}
