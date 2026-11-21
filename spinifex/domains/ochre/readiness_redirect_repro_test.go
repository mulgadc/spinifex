package ochre

//test:in-package: drives the unexported launch wait and reaper sweep against
// local servers with the package's VM, ENI and volume fakes.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// redirectTargetHost is the second server's name; the routing dialer sends
// it to the destination listener and every other address to the source.
const redirectTargetHost = "redirect-target.invalid"

// hitLog records the paths one local server answered.
type hitLog struct {
	mu    sync.Mutex
	paths []string
}

func (h *hitLog) add(path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.paths = append(h.paths, path)
}

func (h *hitLog) count(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, p := range h.paths {
		if p == path {
			n++
		}
	}
	return n
}

// probeRedirectPair is a serving member that redirects every probe to a second
// server, which answers readiness, health and idle vLLM metrics.
type probeRedirectPair struct {
	source, dest *hitLog
	routing      *http.Transport
}

func newProbeRedirectPair(t *testing.T) *probeRedirectPair {
	t.Helper()
	p := &probeRedirectPair{source: &hitLog{}, dest: &hitLog{}}
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.source.add(r.URL.Path)
		http.Redirect(w, r, "http://"+redirectTargetHost+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(src.Close)
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.dest.add(r.URL.Path)
		if r.URL.Path == "/metrics" {
			_, _ = fmt.Fprint(w, "vllm:num_requests_running 0\nvllm:num_requests_waiting 0\nvllm:request_success_total 0\n")
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(dst.Close)

	var d net.Dialer
	p.routing = &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		if host == redirectTargetHost {
			return d.DialContext(ctx, network, dst.Listener.Addr().String())
		}
		return d.DialContext(ctx, network, src.Listener.Addr().String())
	}}
	t.Cleanup(p.routing.CloseIdleConnections)
	return p
}

// following is the client production uses today: net/http's default policy.
func (p *probeRedirectPair) following() *http.Client {
	return &http.Client{Transport: p.routing}
}

// noFollow is the same client refusing redirects, as a rejecting probe would.
func (p *probeRedirectPair) noFollow() *http.Client {
	return &http.Client{
		Transport:     p.routing,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func newRedirectService(t *testing.T, h *launchHarness, client *http.Client) *Service {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	return NewService(nc, ServiceDeps{
		Config:         &config.Config{Region: "us-east-1", AZ: "us-east-1a"},
		Launch:         h.deps(),
		GPU:            sufficientGPU(),
		NodeID:         testNodeID,
		StartupTimeout: 200 * time.Millisecond,
		HTTPClient:     client,
		PollInterval:   5 * time.Millisecond,
		Replicas:       1,
	})
}

func describeState(t *testing.T, s *Service, modelID string) EndpointRecord {
	t.Helper()
	desc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: modelID}, "")
	require.NoError(t, err)
	return desc.Endpoint
}

// redirectClients are the probe clients a redirect must be safe under: the
// default policy production injects, and one that already refuses redirects.
func redirectClients(p *probeRedirectPair) map[string]*http.Client {
	return map[string]*http.Client{"following client": p.following(), "no-follow client": p.noFollow()}
}

func TestReadinessRedirectRepro_LaunchRedirectRetainsWithoutReadyOrTeardown(t *testing.T) {
	t.Parallel()
	for name := range redirectClients(newProbeRedirectPair(t)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newProbeRedirectPair(t)
			h := newLaunchHarness()
			s := newRedirectService(t, h, redirectClients(p)[name])

			_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
			require.NoError(t, err)
			s.WaitLaunches()

			rec := describeState(t, s, testModelID)
			t.Logf("source /v1/models=%d destination /v1/models=%d state=%s instance=%q baseURL=%s terminated=%v enisDeleted=%v volumesDeleted=%v",
				p.source.count("/v1/models"), p.dest.count("/v1/models"), rec.State, rec.InstanceID, rec.BaseURL,
				h.launcher.terminations(), h.vpc.deleted, h.volumes.deleted)
			assert.Positive(t, p.source.count("/v1/models"))
			assert.Zero(t, p.dest.count("/v1/models"), "the redirect target must receive no probe")
			assert.NotEqual(t, StateReady, rec.State, "a redirecting member must not read as ready")
			assert.Equal(t, StateStarting, rec.State, "the launch record must be retained")
			assert.NotEmpty(t, rec.InstanceID, "the retained record must name the VM it holds")
			assert.Empty(t, h.launcher.terminations(), "a redirect must not terminate the VM")
			assert.Empty(t, h.vpc.deleted, "a redirect must not delete the ENI")
			assert.Empty(t, h.volumes.deleted, "a redirect must not delete the weights volume")
		})
	}
}

// redirectReaper is a reaper over a Service whose launch and reaper probes
// share client, with the endpoint record seeded READY rather than launched.
type redirectReaper struct {
	svc     *Service
	reaper  *Reaper
	harness *launchHarness
	key     string
}

func newRedirectReaper(t *testing.T, client *http.Client, rec EndpointRecord) *redirectReaper {
	t.Helper()
	h := newLaunchHarness()
	svc := newRedirectService(t, h, client)
	key := resolveKey(awsidentifiers.GlobalAccountID, rec.ModelID)
	_, err := svc.store.Create(t.Context(), key, &rec)
	require.NoError(t, err)
	return &redirectReaper{
		svc:     svc,
		reaper:  testReaper(t, svc, testNodeID, ReaperDeps{IdleTTL: time.Minute}),
		harness: h,
		key:     key,
	}
}

func (r *redirectReaper) current(t *testing.T) (EndpointRecord, bool) {
	t.Helper()
	rec, _, found, err := r.svc.store.getRevision(t.Context(), r.key)
	require.NoError(t, err)
	return rec, found
}

// seededReady is a READY record for modelID whose one member serves family,
// up and quiet for an hour, with the resources a teardown would release.
func seededReady(modelID, family string, port int) EndpointRecord {
	now := time.Now().UTC()
	return EndpointRecord{
		AccountID:       awsidentifiers.GlobalAccountID,
		ModelID:         modelID,
		State:           StateReady,
		InstanceID:      "i-redirecting",
		ENIID:           "eni-redirecting",
		PrivateIP:       "10.0.0.9",
		BaseURL:         fmt.Sprintf("http://10.0.0.9:%d", port),
		WeightsVolumeID: "vol-redirecting",
		Members: map[string]MemberEndpoint{modelID: {
			Port: port, WeightsVolumeID: "vol-redirecting", Family: family,
		}},
		CreatedAt:    now.Add(-2 * time.Hour),
		ReadyAt:      now.Add(-2 * time.Hour),
		LastActiveAt: now.Add(-2 * time.Hour),
		Generation:   1,
	}
}

func TestReadinessRedirectRepro_ReaperRedirectHoldsWithoutActivityOrTeardown(t *testing.T) {
	t.Parallel()
	probes := []struct {
		name    string
		modelID string
		family  string
		port    int
		path    string
	}{
		{"metrics scrape", testModelID, FamilyMeta, vllmServePort, "/metrics"},
		{"liveness probe", "embed-only-bundle", FamilyTEI, teiBasePort, "/health"},
	}
	for _, probe := range probes {
		for name := range redirectClients(newProbeRedirectPair(t)) {
			t.Run(probe.name+"/"+name, func(t *testing.T) {
				t.Parallel()
				p := newProbeRedirectPair(t)
				seeded := seededReady(probe.modelID, probe.family, probe.port)
				r := newRedirectReaper(t, redirectClients(p)[name], seeded)

				for range maxScrapeFailures + 1 {
					require.NoError(t, r.reaper.sweepOnce(t.Context(), nil))
				}
				r.svc.WaitLaunches()

				got, found := r.current(t)
				t.Logf("source %s=%d destination %s=%d recordFound=%v state=%s scrapeFailures=%d lastActiveRefreshed=%v terminated=%v enisDeleted=%v volumesDeleted=%v relaunches=%d",
					probe.path, p.source.count(probe.path), probe.path, p.dest.count(probe.path), found, got.State,
					got.ScrapeFailures, got.LastActiveAt.After(seeded.LastActiveAt),
					r.harness.launcher.terminations(), r.harness.vpc.deleted, r.harness.volumes.deleted,
					r.harness.launcher.launchCount.Load())
				assert.Positive(t, p.source.count(probe.path))
				assert.Zero(t, p.dest.count(probe.path), "the redirect target must receive no probe")
				assert.True(t, found, "a redirecting endpoint's record must be retained")
				assert.Equal(t, StateReady, got.State)
				assert.Zero(t, got.ScrapeFailures, "a redirect must not count toward escalation")
				assert.False(t, got.LastActiveAt.After(seeded.LastActiveAt), "a redirect must not count as activity")
				assert.Empty(t, r.harness.launcher.terminations(), "a redirect must not terminate the VM")
				assert.Empty(t, r.harness.vpc.deleted, "a redirect must not delete the ENI")
				assert.Empty(t, r.harness.volumes.deleted, "a redirect must not delete the weights volume")
				assert.Zero(t, r.harness.launcher.launchCount.Load(), "a redirect must not relaunch")
			})
		}
	}
}
