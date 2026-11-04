package floating

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/providers/cloud/latitude"
)

func latitudeFake() *latitude.Fake {
	f := latitude.NewFake("sv_a", "sv_b")
	f.Seed(latitude.ElasticIP{ID: "eip_1", Address: netip.MustParseAddr("203.0.113.1")})
	f.Seed(latitude.ElasticIP{ID: "eip_2", Address: netip.MustParseAddr("203.0.113.2"), ServerID: "sv_a"})
	f.Seed(latitude.ElasticIP{ID: "eip_other", Address: netip.MustParseAddr("203.0.113.9")})
	return f
}

func TestLatitudeProvider_MembersAreOnlyTheConfiguredIDs(t *testing.T) {
	p, err := NewLatitudeProvider(latitudeFake(), []string{"eip_1", "eip_2"})
	require.NoError(t, err)
	got, err := p.Members(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "eip_1", got[0].ID)
	assert.Equal(t, "sv_a", got[1].HolderID)
	assert.Empty(t, got[0].Label)
}

// The project listing is never read, so a project holding more addresses than a listing
// page returns does not affect a pool of two.
func TestLatitudeProvider_MembersIgnoreTheSizeOfTheProject(t *testing.T) {
	f := latitudeFake()
	for i := range 150 {
		f.Seed(latitude.ElasticIP{ID: fmt.Sprintf("eip_x%03d", i), Address: netip.AddrFrom4([4]byte{198, 51, 100, byte(i)})})
	}
	p, err := NewLatitudeProvider(f, []string{"eip_1", "eip_2"})
	require.NoError(t, err)
	got, err := p.Members(context.Background())
	require.NoError(t, err)
	assert.Len(t, got, 2)
	assert.Equal(t, []string{"get", "get"}, f.Calls)
}

func TestLatitudeProvider_AMissingMemberIsLoggedAndLeftOut(t *testing.T) {
	buf := captureLog(t)
	p, err := NewLatitudeProvider(latitudeFake(), []string{"eip_gone", "eip_1"})
	require.NoError(t, err)
	got, err := p.Members(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "eip_1", got[0].ID)
	assert.Contains(t, buf.String(), "eip_gone")
}

func TestLatitudeProvider_AnyOtherReadFailureFailsTheCall(t *testing.T) {
	f := latitudeFake()
	f.Fail["get"] = errors.New("timeout")
	p, err := NewLatitudeProvider(f, []string{"eip_1", "eip_2"})
	require.NoError(t, err)
	_, err = p.Members(context.Background())
	assert.ErrorContains(t, err, "eip_1")
}

// One address gone from latitude.sh must not stop the others from being claimed.
func TestLatitudeProvider_EngineSurvivesAMissingMember(t *testing.T) {
	f := latitudeFake()
	p, err := NewLatitudeProvider(f, []string{"eip_gone", "eip_1"})
	require.NoError(t, err)
	n := latitudeNode(t, p, newMemStore(), "sv_b")
	ip := allocENI(t, n, "eni-1")
	assert.Equal(t, netip.MustParseAddr("203.0.113.1"), ip)
	n.plug("eni-1")
	res, err := n.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{ip.String()}, res.Claimed)
	assert.Equal(t, "sv_b", f.HolderOf("eip_1"))
}

// An address the provider reports in error is not handed out, and one already on this node
// is stalled rather than counted as settled.
func TestLatitudeProvider_AnErrorStateAddressIsNeitherAllocatedNorSettled(t *testing.T) {
	f := latitudeFake()
	f.Seed(latitude.ElasticIP{ID: "eip_1", Address: netip.MustParseAddr("203.0.113.1"), Status: latitude.StatusError})
	p, err := NewLatitudeProvider(f, []string{"eip_1", "eip_2"})
	require.NoError(t, err)
	store := newMemStore()
	n := latitudeNode(t, p, store, "sv_b")
	ip := allocENI(t, n, "eni-1")
	assert.Equal(t, netip.MustParseAddr("203.0.113.2"), ip, "eip_1 is skipped")

	n.plug("eni-1")
	res, err := n.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{ip.String()}, res.Claimed)

	f.Seed(latitude.ElasticIP{ID: "eip_2", Address: ip, ServerID: "sv_b", Status: latitude.StatusError})
	res, err = n.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Stalled, 1)
	assert.Equal(t, ip.String(), res.Stalled[0].PublicIP)
	assert.Contains(t, res.Stalled[0].Reason, "error state")
}

func latitudeNode(t *testing.T, p Provider, store Store, host string) *node {
	t.Helper()
	n := &node{plugged: map[string]struct{}{}, gateway: map[string]bool{}}
	alloc, err := New(p, store, Config{
		Pool:       external.ExternalPoolConfig{Name: pool, Source: "latitude"},
		HostID:     host,
		LocalPorts: func(context.Context) (map[string]struct{}, error) { return n.plugged, nil },
	})
	require.NoError(t, err)
	n.alloc = alloc
	return n
}

func TestLatitudeProvider_AttachMovesAndDetachDeleteChangeNothing(t *testing.T) {
	f := latitudeFake()
	p, err := NewLatitudeProvider(f, []string{"eip_1", "eip_2"})
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, p.Attach(ctx, "eip_2", "sv_b"))
	assert.Equal(t, "sv_b", f.HolderOf("eip_2"))
	require.NoError(t, p.Detach(ctx, "eip_2"))
	require.NoError(t, p.Delete(ctx, "eip_2"))
	assert.Equal(t, "sv_b", f.HolderOf("eip_2"))
	got, err := p.Get(ctx, "eip_2")
	require.NoError(t, err)
	assert.Equal(t, "sv_b", got.HolderID)
}

func TestLatitudeProvider_RefusesWhatIsNotInTheSet(t *testing.T) {
	f := latitudeFake()
	p, err := NewLatitudeProvider(f, []string{"eip_1"})
	require.NoError(t, err)
	ctx := context.Background()

	assert.ErrorIs(t, p.Attach(ctx, "eip_other", "sv_b"), ErrNotFound)
	assert.Empty(t, f.HolderOf("eip_other"))
	_, err = p.Get(ctx, "eip_other")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = p.Create(ctx, "x")
	assert.Error(t, err)
}

func TestLatitudeProvider_MapsNotFoundAndKeepsRefusals(t *testing.T) {
	f := latitudeFake()
	p, err := NewLatitudeProvider(f, []string{"eip_1"})
	require.NoError(t, err)
	ctx := context.Background()

	assert.ErrorIs(t, p.Attach(ctx, "eip_1", "sv_unknown"), latitude.ErrMoveRefused)
	f.Fail["get"] = latitude.ErrNotFound
	_, err = p.Get(ctx, "eip_1")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestNewLatitudeProvider_NeedsIDs(t *testing.T) {
	_, err := NewLatitudeProvider(latitudeFake(), nil)
	assert.Error(t, err)
}

// The engine over the adapter: allocation picks, the owner's node claims, and
// running out is the capacity error.
func TestLatitudeProvider_EngineEndToEnd(t *testing.T) {
	f := latitudeFake()
	p, err := NewLatitudeProvider(f, []string{"eip_1", "eip_2"})
	require.NoError(t, err)
	store := newMemStore()
	n := &node{plugged: map[string]struct{}{}, gateway: map[string]bool{}}
	alloc, err := New(p, store, Config{
		Pool:       external.ExternalPoolConfig{Name: pool, Source: "latitude"},
		HostID:     "sv_b",
		LocalPorts: func(context.Context) (map[string]struct{}, error) { return n.plugged, nil },
	})
	require.NoError(t, err)
	n.alloc = alloc

	ip := allocENI(t, n, "eni-1")
	assert.Equal(t, netip.MustParseAddr("203.0.113.1"), ip)
	n.plug("eni-1")
	res, err := alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{ip.String()}, res.Claimed)
	assert.Equal(t, "sv_b", f.HolderOf("eip_1"))

	allocENI(t, n, "eni-2")
	_, err = alloc.Allocate(context.Background(), external.AllocateRequest{PoolName: pool, Purpose: "eni-public", ENIID: "eni-3"})
	assert.ErrorContains(t, err, "InsufficientAddressCapacity")
}

func TestProviderFor_PicksByPoolSource(t *testing.T) {
	p, err := providerFor(external.ExternalPoolConfig{
		Name: pool, Source: external.SourceLatitude, LatitudeProjectID: "proj_1", LatitudeElasticIPIDs: []string{"eip_1"},
	})
	require.NoError(t, err)
	_, isFixed := p.(FixedSet)
	assert.True(t, isFixed)

	v, err := providerFor(external.ExternalPoolConfig{Name: pool, Source: external.SourceVultr, VultrRegion: "ord"})
	require.NoError(t, err)
	_, isFixed = v.(FixedSet)
	assert.False(t, isFixed)

	_, err = providerFor(external.ExternalPoolConfig{Name: pool, Source: external.SourceStatic})
	assert.ErrorContains(t, err, "not a floating provider")
	_, err = providerFor(external.ExternalPoolConfig{Name: pool, Source: external.SourceLatitude, LatitudeProjectID: "p"})
	assert.Error(t, err, "a latitude pool with no IDs is refused")
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// deploy-vultr.sh waits for this exact text after a restart.
func TestReportFirstCall_VultrLogTextIsPinned(t *testing.T) {
	buf := captureLog(t)
	reportFirstCall(context.Background(), external.ExternalPoolConfig{Name: pool, Source: external.SourceVultr, VultrRegion: "ord"}, nil)
	assert.Contains(t, buf.String(), `msg="floating using Vultr reserved IPs"`)

	buf.Reset()
	reportFirstCall(context.Background(), external.ExternalPoolConfig{Name: pool, Source: external.SourceVultr}, errors.New("refused"))
	assert.Contains(t, buf.String(), `msg="floating: Vultr refused the pool's first call; public addresses will fail until fixed"`)
}

func TestReportFirstCall_LatitudeLogsItsOwnLine(t *testing.T) {
	buf := captureLog(t)
	reportFirstCall(context.Background(), external.ExternalPoolConfig{Name: pool, Source: external.SourceLatitude}, nil)
	assert.Contains(t, buf.String(), `msg="floating pool ready"`)
	assert.NotContains(t, buf.String(), "Vultr")
}

func TestLatitudeProvider_AnUnusableMemberIsAConfigError(t *testing.T) {
	f := latitudeFake()
	f.Seed(latitude.ElasticIP{ID: "eip_bgp", Address: netip.MustParseAddr("203.0.113.50"), Mode: "bgp", Family: "IPv4", PrefixLength: 24})
	p, err := NewLatitudeProvider(f, []string{"eip_1", "eip_bgp"})
	require.NoError(t, err)
	_, err = p.Members(context.Background())
	assert.ErrorContains(t, err, "eip_bgp")
	assert.ErrorContains(t, err, "routed IPv4 /32")
}

// A response can put anything in server.id. It becomes the holder the engine logs as
// from_host after a move, and only a host we configured may be named there.
func TestHolderPass_UnrecognisedHolderIDNeverReachesTheLog(t *testing.T) {
	const token = "FAKESECRET"
	var mu sync.Mutex
	holder := token // what the API reports as the holder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPatch {
			holder = "sv_bbbbbb"
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, "{}")
			return
		}
		item := `{"id":"eip_aaaaaa","attributes":{"address":"203.0.113.1","status":"active","mode":"routed","family":"IPv4","prefix_length":32,"server":{"id":"` + holder + `"}}}`
		if strings.HasSuffix(r.URL.Path, "/eip_aaaaaa") {
			_, _ = io.WriteString(w, `{"data":`+item+`}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[`+item+`]}`)
	}))
	t.Cleanup(srv.Close)
	keyFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(keyFile, []byte(token), 0o600))
	client, err := latitude.New(latitude.Config{BaseURL: srv.URL, KeyFile: keyFile, ProjectID: "p", HTTPClient: srv.Client(), PollInterval: time.Millisecond})
	require.NoError(t, err)
	p, err := NewLatitudeProvider(client, []string{"eip_aaaaaa"})
	require.NoError(t, err)

	n := &node{plugged: map[string]struct{}{}, gateway: map[string]bool{}}
	alloc, err := New(p, newMemStore(), Config{
		Pool:       external.ExternalPoolConfig{Name: pool, Source: external.SourceLatitude},
		HostID:     "sv_bbbbbb",
		LocalPorts: func(context.Context) (map[string]struct{}, error) { return n.plugged, nil },
	})
	require.NoError(t, err)
	n.alloc = alloc
	allocENI(t, n, "eni-1")
	n.plug("eni-1")

	logs := captureLog(t)
	res, err := alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	assert.Len(t, res.Claimed, 1)
	assert.Contains(t, logs.String(), "floating claimed an address", "the move is logged")
	assert.NotContains(t, logs.String(), token)
	assert.Contains(t, logs.String(), "unrecognised holder")
}

// A short server ID that is ours is a real holder: the address is settled, with no move.
func TestHolderPass_ShortConfiguredHolderIDIsSettled(t *testing.T) {
	f := latitudeFake()
	f.Seed(latitude.ElasticIP{ID: "eip_1", Address: netip.MustParseAddr("203.0.113.1"), ServerID: "sv_a"})
	p, err := NewLatitudeProvider(f, []string{"eip_1"})
	require.NoError(t, err)
	n := &node{plugged: map[string]struct{}{}, gateway: map[string]bool{}}
	alloc, err := New(p, newMemStore(), Config{
		Pool:       external.ExternalPoolConfig{Name: pool, Source: external.SourceLatitude},
		HostID:     "sv_a",
		LocalPorts: func(context.Context) (map[string]struct{}, error) { return n.plugged, nil },
	})
	require.NoError(t, err)
	n.alloc = alloc
	allocENI(t, n, "eni-1")
	n.plug("eni-1")

	_, err = alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	assert.NotContains(t, f.Calls, "move")
	assert.Equal(t, "sv_a", f.HolderOf("eip_1"))
}

func TestHolderLabel_NamesOnlyConfiguredHosts(t *testing.T) {
	a := &PoolAllocator{fixed: true, cfg: Config{HostID: "sv_a", KnownHostIDs: []string{"sv_b"}}}
	assert.Equal(t, "sv_a", a.holderLabel("sv_a"))
	assert.Equal(t, "sv_b", a.holderLabel("sv_b"))
	assert.Empty(t, a.holderLabel(""))
	assert.Equal(t, "unrecognised holder", a.holderLabel("FAKESECRET"))
	other := &PoolAllocator{cfg: Config{HostID: "sv_a"}}
	assert.Equal(t, "whatever-the-provider-says", other.holderLabel("whatever-the-provider-says"))
}
