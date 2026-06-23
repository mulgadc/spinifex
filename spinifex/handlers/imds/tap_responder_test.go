package handlers_imds

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loopbackTapListen returns a tapListenFunc backed by a real 127.0.0.1 listener
// so the responder runs end-to-end (http.Server.Serve + the per-tap BaseContext)
// without root or the link-local address. It records the bound address per
// endpoint so the test can dial it.
func loopbackTapListen(addrs *sync.Map) tapListenFunc {
	return func(_ context.Context, endpoint string) (net.Listener, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		addrs.Store(endpoint, ln.Addr().String())
		return ln, nil
	}
}

// staticResolve returns a resolveENIFunc serving a fixed eniID → facts table.
func staticResolve(table map[string]*eniFacts) resolveENIFunc {
	return func(eniID string) (*eniFacts, error) { return table[eniID], nil }
}

func TestTapResponder_ThreadsENIIdentity(t *testing.T) {
	const (
		eniID    = "eni-aaa11111"
		endpoint = "ime-aaa11111"
	)
	resolved := &eniFacts{eniID: eniID, instanceID: "i-aaaa1111"}

	// Echo the ENI the responder threaded into the request context. The localport
	// path's (vpcID, srcIP) keys must never be consulted on the per-tap path.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		eni, _ := r.Context().Value(ctxKeyENI).(*eniFacts)
		require.NotNil(t, eni)
		_, _ = io.WriteString(w, eni.eniID+"|"+eni.instanceID)
	})

	var addrs sync.Map
	m := newTapResponderManager(handler, staticResolve(map[string]*eniFacts{eniID: resolved}), loopbackTapListen(&addrs))
	ctx := context.Background()

	require.NoError(t, m.start(ctx, eniID, endpoint))
	// Second start of the same ENI is a no-op: no second listener bound.
	require.NoError(t, m.start(ctx, eniID, endpoint))

	raw, ok := addrs.Load(endpoint)
	require.True(t, ok)
	addr, ok := raw.(string)
	require.True(t, ok)

	resp, err := http.Get("http://" + addr + prefixMetaData)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, eniID+"|i-aaaa1111", string(body), "handler must see the responder's ENI identity")

	m.stop(eniID)
	client := http.Client{Timeout: 500 * time.Millisecond}
	_, err = client.Get("http://" + addr + prefixMetaData)
	assert.Error(t, err, "listener must be closed after stop")
}

// The full IMDSv2 flow over a per-tap responder, reusing the production handler
// verbatim: token issuance, token-gated GET, and the identity it returns all come
// from the tap's ENI — never the source IP. Two responders prove overlapping-CIDR
// isolation (both guests dial from 127.0.0.1) and per-ENI token binding.
func TestTapResponder_IMDSv2PerTapIdentity(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{}, &fakeIAM{}, &fakeAssumer{})
	table := map[string]*eniFacts{
		"eni-aaa11111": {eniID: "eni-aaa11111", instanceID: "i-aaaa1111"},
		"eni-bbb22222": {eniID: "eni-bbb22222", instanceID: "i-bbbb2222"},
	}

	var addrs sync.Map
	m := newTapResponderManager(svc.httpHandler(), staticResolve(table), loopbackTapListen(&addrs))
	ctx := context.Background()
	require.NoError(t, m.start(ctx, "eni-aaa11111", "ime-aaa11111"))
	require.NoError(t, m.start(ctx, "eni-bbb22222", "ime-bbb22222"))
	defer m.shutdown()

	addrA := mustAddr(t, &addrs, "ime-aaa11111")
	addrB := mustAddr(t, &addrs, "ime-bbb22222")

	// Responder A: PUT token then token-gated GET returns A's instance-id.
	tokenA := putTapToken(t, addrA)
	assert.Equal(t, http.StatusUnauthorized, tapGet(t, addrA, prefixMetaData+"instance-id", "").code,
		"tokenless GET must be 401")
	a := tapGet(t, addrA, prefixMetaData+"instance-id", tokenA)
	assert.Equal(t, http.StatusOK, a.code)
	assert.Equal(t, "i-aaaa1111", a.body)

	// Responder B resolves to its own ENI from the same source IP.
	tokenB := putTapToken(t, addrB)
	b := tapGet(t, addrB, prefixMetaData+"instance-id", tokenB)
	assert.Equal(t, "i-bbbb2222", b.body, "identity comes from the tap, not the source IP")

	// A token binds to its tap's ENI: presenting A's token at responder B is 401.
	assert.Equal(t, http.StatusUnauthorized, tapGet(t, addrB, prefixMetaData+"instance-id", tokenA).code,
		"token must not validate against another tap's ENI")
}

// reconcile converges the active responders to the live tap set: it starts a
// responder for a newly attached tap, leaves an unchanged one alone, and stops a
// responder whose tap has gone — the vpcd reconcile-from-taps contract.
func TestTapResponder_ReconcileConvergesToLiveSet(t *testing.T) {
	table := map[string]*eniFacts{
		"eni-aaa11111": {eniID: "eni-aaa11111", instanceID: "i-aaaa1111"},
		"eni-bbb22222": {eniID: "eni-bbb22222", instanceID: "i-bbbb2222"},
	}
	var addrs sync.Map
	m := newTapResponderManager(http.NewServeMux(), staticResolve(table), loopbackTapListen(&addrs))
	ctx := context.Background()

	// First pass: only A is live, so only A serves.
	m.reconcile(ctx, map[string]string{"eni-aaa11111": "ime-aaa11111"})
	assertActive(t, m, "eni-aaa11111")
	assertNotActive(t, m, "eni-bbb22222")

	// Second pass: A stays, B is added. A must not be re-bound (idempotent), B starts.
	addrA := mustAddr(t, &addrs, "ime-aaa11111")
	m.reconcile(ctx, map[string]string{"eni-aaa11111": "ime-aaa11111", "eni-bbb22222": "ime-bbb22222"})
	assertActive(t, m, "eni-aaa11111", "eni-bbb22222")
	assert.Equal(t, addrA, mustAddr(t, &addrs, "ime-aaa11111"), "A's listener must not be rebound")

	// Third pass: A's tap is gone (terminate). Its responder is stopped; B stays.
	m.reconcile(ctx, map[string]string{"eni-bbb22222": "ime-bbb22222"})
	assertNotActive(t, m, "eni-aaa11111")
	assertActive(t, m, "eni-bbb22222")
}

func TestTapResponder_StartRejectsMissAndError(t *testing.T) {
	var addrs sync.Map
	listen := loopbackTapListen(&addrs)

	// A resolve miss (ENI record not yet visible) is an error so the caller retries,
	// and no listener is bound.
	miss := newTapResponderManager(http.NewServeMux(), staticResolve(nil), listen)
	require.Error(t, miss.start(context.Background(), "eni-gone", "ime-gone"))
	_, bound := addrs.Load("ime-gone")
	assert.False(t, bound, "no listener may be bound on a resolve miss")

	// A resolve backend error propagates.
	boom := newTapResponderManager(http.NewServeMux(),
		func(string) (*eniFacts, error) { return nil, errors.New("kv down") }, listen)
	require.Error(t, boom.start(context.Background(), "eni-x", "ime-x"))
}

func TestTapResponder_StartValidatesArgs(t *testing.T) {
	m := newTapResponderManager(http.NewServeMux(), staticResolve(nil), loopbackTapListen(&sync.Map{}))
	require.Error(t, m.start(context.Background(), "", "ime-x"))
	require.Error(t, m.start(context.Background(), "eni-x", ""))
}

// resolveCaller treats a context-threaded ENI as authoritative, ignoring the
// (vpcID, srcIP) localport keys entirely — even a bogus VPC in context.
func TestResolveCaller_PerTapENIWins(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{eniErr: errors.New("must not be called")}, &fakeIAM{}, &fakeAssumer{})
	want := &eniFacts{eniID: "eni-tap", instanceID: "i-tap"}

	req := httptest.NewRequest(http.MethodGet, "http://"+MetaDataServerIP+prefixMetaData, nil)
	req.RemoteAddr = "10.9.9.9:40000"
	ctx := context.WithValue(req.Context(), ctxKeyVPCID, "vpc-bogus")
	ctx = context.WithValue(ctx, ctxKeyENI, want)
	req = req.WithContext(ctx)

	got := svc.resolveCaller(req)
	require.NotNil(t, got)
	assert.Equal(t, "eni-tap", got.eniID, "threaded ENI must win over the localport path")
}

// ----- helpers ----------------------------------------------------------

type tapResult struct {
	code int
	body string
}

// assertActive asserts exactly the given ENIs have a running responder.
func assertActive(t *testing.T, m *tapResponderManager, eniIDs ...string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range eniIDs {
		if _, ok := m.active[id]; !ok {
			t.Errorf("responder for %s must be active", id)
		}
	}
}

// assertNotActive asserts none of the given ENIs have a running responder.
func assertNotActive(t *testing.T, m *tapResponderManager, eniIDs ...string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range eniIDs {
		if _, ok := m.active[id]; ok {
			t.Errorf("responder for %s must not be active", id)
		}
	}
}

func mustAddr(t *testing.T, addrs *sync.Map, endpoint string) string {
	t.Helper()
	raw, ok := addrs.Load(endpoint)
	require.True(t, ok, "endpoint %s not bound", endpoint)
	addr, ok := raw.(string)
	require.True(t, ok)
	return addr
}

func putTapToken(t *testing.T, addr string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, "http://"+addr+pathToken, nil)
	require.NoError(t, err)
	req.Header.Set(hdrTokenTTL, "60")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotEmpty(t, string(body))
	return string(body)
}

func tapGet(t *testing.T, addr, path, token string) tapResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set(hdrToken, token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return tapResult{code: resp.StatusCode, body: string(body)}
}
