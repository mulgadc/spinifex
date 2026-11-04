package latitude

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// api serves canned responses in order per "METHOD path", and records bodies and tokens.
type api struct {
	t       *testing.T
	mu      sync.Mutex
	replies map[string][]reply
	bodies  []string
	auth    []string
	queries []string
}

const testToken = "FAKESECRET"

type reply struct {
	status int
	body   string
}

func eip(id, addr, status, server string) string {
	srv := "null"
	if server != "" {
		srv = fmt.Sprintf(`{"id":%q}`, server)
	}
	idv := "null"
	if id != "" {
		idv = fmt.Sprintf("%q", id)
	}
	return fmt.Sprintf(`{"id":%s,"type":"elastic_ips","attributes":{"address":%q,"status":%q,"mode":"routed","family":"IPv4","prefix_length":32,"server":%s}}`, idv, addr, status, srv)
}

// bgp is the documented shape of a BGP-mode address, which cannot be moved.
func bgp(id, addr string) string {
	return fmt.Sprintf(`{"id":%q,"type":"elastic_ips","attributes":{"address":%q,"status":"active","mode":"bgp","family":"IPv4","prefix_length":24,"server":null}}`, id, addr)
}

func one(id, addr, status, server string) string {
	return `{"data":` + eip(id, addr, status, server) + `}`
}

func errBody(code string) string {
	return fmt.Sprintf(`{"errors":[{"code":%q,"title":"t","detail":"d %s"}]}`, code, code)
}

func newClient(t *testing.T, replies map[string][]reply) (*api, *Client) {
	t.Helper()
	a := &api{t: t, replies: replies}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	keyFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(keyFile, []byte(testToken+"\n"), 0o600))
	c, err := New(Config{
		BaseURL: srv.URL, KeyFile: keyFile, ProjectID: "proj_1",
		Backoff: time.Millisecond, PollInterval: time.Millisecond, MoveTimeout: time.Second,
	})
	require.NoError(t, err)
	return a, c
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	a.auth = append(a.auth, r.Header.Get("Authorization"))
	a.queries = append(a.queries, r.URL.RawQuery)
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		a.bodies = append(a.bodies, string(raw))
	}
	q := a.replies[key]
	if len(q) == 0 {
		a.t.Errorf("unexpected call %s", key)
		w.WriteHeader(http.StatusTeapot)
		return
	}
	next := q[0]
	if len(q) > 1 {
		a.replies[key] = q[1:]
	}
	w.WriteHeader(next.status)
	_, _ = io.WriteString(w, next.body)
}

func TestGet_SendsTheTokenAndReadsTheHolder(t *testing.T) {
	a, c := newClient(t, map[string][]reply{"GET /elastic_ips/eip_000001": {{200, one("eip_000001", "203.0.113.1", "active", "sv_000001")}}})
	ip, err := c.GetElasticIP(t.Context(), "eip_000001")
	require.NoError(t, err)
	assert.Equal(t, "sv_000001", ip.ServerID)
	assert.Equal(t, []string{"Bearer " + testToken}, a.auth)
}

func TestGet_NullIDIsRejected(t *testing.T) {
	_, c := newClient(t, map[string][]reply{"GET /elastic_ips/eip_000001": {{200, one("", "203.0.113.1", "configuring", "")}}})
	_, err := c.GetElasticIP(t.Context(), "eip_000001")
	assert.ErrorContains(t, err, "no id")
}

func TestGet_NotFound(t *testing.T) {
	_, c := newClient(t, map[string][]reply{"GET /elastic_ips/eip_000001": {{404, errBody("NOT_FOUND")}}})
	_, err := c.GetElasticIP(t.Context(), "eip_000001")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestMove_PatchesThenWaitsForActive(t *testing.T) {
	a, c := newClient(t, map[string][]reply{
		"GET /elastic_ips/eip_000001": {
			{200, one("eip_000001", "203.0.113.1", "active", "sv_000001")},
			{200, one("eip_000001", "203.0.113.1", "moving", "sv_000001")},
			{200, one("eip_000001", "203.0.113.1", "active", "sv_000002")},
		},
		"PATCH /elastic_ips/eip_000001": {{202, one("eip_000001", "203.0.113.1", "moving", "sv_000001")}},
	})
	require.NoError(t, c.MoveElasticIP(t.Context(), "eip_000001", "sv_000002"))

	require.Len(t, a.bodies, 1)
	var body struct {
		Data struct {
			Type       string            `json:"type"`
			Attributes map[string]string `json:"attributes"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(a.bodies[0]), &body))
	assert.Equal(t, "elastic_ips", body.Data.Type)
	assert.Equal(t, map[string]string{"server_id": "sv_000002"}, body.Data.Attributes)
}

func TestMove_AlreadyThereMakesNoPatch(t *testing.T) {
	_, c := newClient(t, map[string][]reply{
		"GET /elastic_ips/eip_000001": {{200, one("eip_000001", "203.0.113.1", "active", "sv_000002")}},
	})
	require.NoError(t, c.MoveElasticIP(t.Context(), "eip_000001", "sv_000002"))
}

// The caller checked ownership just before calling, so a call must never wait and then write.
func TestMove_NotActiveStateMakesNoWrite(t *testing.T) {
	a, c := newClient(t, map[string][]reply{
		"GET /elastic_ips/eip_000001": {{200, one("eip_000001", "203.0.113.1", "moving", "sv_000001")}},
	})
	err := c.MoveElasticIP(t.Context(), "eip_000001", "sv_000002")
	assert.ErrorIs(t, err, ErrNotActive)
	assert.Empty(t, a.bodies, "no PATCH was sent")
}

func TestMove_RefusedAsNotActiveIsNotRetried(t *testing.T) {
	a, c := newClient(t, map[string][]reply{
		"GET /elastic_ips/eip_000001":   {{200, one("eip_000001", "203.0.113.1", "active", "sv_000001")}},
		"PATCH /elastic_ips/eip_000001": {{422, errBody("ELASTIC_IP_NOT_ACTIVE")}},
	})
	err := c.MoveElasticIP(t.Context(), "eip_000001", "sv_000002")
	assert.ErrorIs(t, err, ErrNotActive)
	assert.Len(t, a.bodies, 1, "exactly one PATCH")
}

func TestMove_NeverIssuesASecondWriteWhileWaiting(t *testing.T) {
	a, c := newClient(t, map[string][]reply{
		"GET /elastic_ips/eip_000001": {
			{200, one("eip_000001", "203.0.113.1", "active", "sv_000001")},
			{200, one("eip_000001", "203.0.113.1", "moving", "sv_000001")},
			{200, one("eip_000001", "203.0.113.1", "active", "sv_000001")}, // another node moved it back meanwhile
			{200, one("eip_000001", "203.0.113.1", "active", "sv_000002")},
		},
		"PATCH /elastic_ips/eip_000001": {{202, one("eip_000001", "203.0.113.1", "moving", "sv_000001")}},
	})
	require.NoError(t, c.MoveElasticIP(t.Context(), "eip_000001", "sv_000002"))
	assert.Len(t, a.bodies, 1, "waiting only reads")
}

func TestMove_BGPAddressIsRefusedWithoutAWrite(t *testing.T) {
	a, c := newClient(t, map[string][]reply{
		"GET /elastic_ips/eip_bbbbbb": {{200, `{"data":` + bgp("eip_bbbbbb", "203.0.113.9") + `}`}},
	})
	err := c.MoveElasticIP(t.Context(), "eip_bbbbbb", "sv_000002")
	assert.ErrorIs(t, err, ErrMoveRefused)
	assert.Empty(t, a.bodies)
}

func TestUnusable(t *testing.T) {
	ok := ElasticIP{Mode: "routed", Family: "IPv4", PrefixLength: 32}
	assert.Empty(t, ok.Unusable())
	for name, e := range map[string]ElasticIP{
		"bgp":    {Mode: "bgp", Family: "IPv4", PrefixLength: 32},
		"v6":     {Mode: "routed", Family: "IPv6", PrefixLength: 32},
		"prefix": {Mode: "routed", Family: "IPv4", PrefixLength: 24},
		"empty":  {},
	} {
		assert.NotEmpty(t, e.Unusable(), name)
	}
}

func TestErrors_NeverCarryTheBodyOrTheToken(t *testing.T) {
	bodies := map[string]string{
		"json detail": `{"errors":[{"code":"BAD_THING","title":"x","detail":"echo ` + testToken + `"}]}`,
		"json code":   `{"errors":[{"code":"` + testToken + `","detail":"y"}]}`,
		"plain text":  "Authorization: Bearer " + testToken,
		"html":        "<html>" + testToken + "</html>",
	}
	for name, body := range bodies {
		for _, status := range []int{400, 404, 422, 401} {
			t.Run(fmt.Sprintf("%s %d", name, status), func(t *testing.T) {
				_, c := newClient(t, map[string][]reply{"GET /elastic_ips/eip_000001": {{status, body}}})
				_, err := c.GetElasticIP(t.Context(), "eip_000001")
				require.Error(t, err)
				assert.NotContains(t, err.Error(), testToken)
				assert.NotContains(t, fmt.Sprintf("%+v", err), testToken)
			})
		}
	}
}

func TestErrors_KeepTheStatusAndAKnownCode(t *testing.T) {
	_, c := newClient(t, map[string][]reply{"GET /elastic_ips/eip_000001": {{422, errBody("ELASTIC_IP_NOT_ACTIVE")}}})
	_, err := c.GetElasticIP(t.Context(), "eip_000001")
	assert.EqualError(t, err, "latitude: HTTP 422 ELASTIC_IP_NOT_ACTIVE")
}

func TestErrors_ADecodeFailureDoesNotEchoTheBody(t *testing.T) {
	_, c := newClient(t, map[string][]reply{"GET /elastic_ips/eip_000001": {{200, `{"data": ` + testToken}}})
	_, err := c.GetElasticIP(t.Context(), "eip_000001")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testToken)
}

func TestMove_RefusalsAreFinal(t *testing.T) {
	for _, code := range []string{"SERVER_NOT_IN_PROJECT", "SITE_MISMATCH", "SERVER_NETWORK_INCOMPATIBLE"} {
		t.Run(code, func(t *testing.T) {
			_, c := newClient(t, map[string][]reply{
				"GET /elastic_ips/eip_000001":   {{200, one("eip_000001", "203.0.113.1", "active", "sv_000001")}},
				"PATCH /elastic_ips/eip_000001": {{422, errBody(code)}},
			})
			err := c.MoveElasticIP(t.Context(), "eip_000001", "sv_000002")
			assert.ErrorIs(t, err, ErrMoveRefused)
			assert.ErrorContains(t, err, code)
		})
	}
}

func TestMove_ErrorStateIsRefused(t *testing.T) {
	_, c := newClient(t, map[string][]reply{"GET /elastic_ips/eip_000001": {{200, one("eip_000001", "203.0.113.1", "error", "")}}})
	assert.ErrorIs(t, c.MoveElasticIP(t.Context(), "eip_000001", "sv_000002"), ErrMoveRefused)
}

func TestMove_NeverSettlingTimesOut(t *testing.T) {
	_, c := newClient(t, map[string][]reply{
		"GET /elastic_ips/eip_000001": {
			{200, one("eip_000001", "203.0.113.1", "active", "sv_000001")},
			{200, one("eip_000001", "203.0.113.1", "moving", "sv_000001")},
		},
		"PATCH /elastic_ips/eip_000001": {{202, one("eip_000001", "203.0.113.1", "moving", "sv_000001")}},
	})
	c.cfg.MoveTimeout = 30 * time.Millisecond
	assert.ErrorIs(t, c.MoveElasticIP(t.Context(), "eip_000001", "sv_000002"), ErrNotSettled)
}

func TestDo_RetriesAThrottleThenSucceeds(t *testing.T) {
	_, c := newClient(t, map[string][]reply{"GET /elastic_ips/eip_000001": {
		{429, `{"meta":{"retry_after":1}}`},
		{200, one("eip_000001", "203.0.113.1", "active", "")},
	}})
	ip, err := c.GetElasticIP(t.Context(), "eip_000001")
	require.NoError(t, err)
	assert.Equal(t, "eip_000001", ip.ID)
}

func TestDo_RefusedTokenIsUnauthorized(t *testing.T) {
	_, c := newClient(t, map[string][]reply{"GET /elastic_ips/eip_000001": {{401, errBody("UNAUTHORIZED")}}})
	_, err := c.GetElasticIP(t.Context(), "eip_000001")
	assert.ErrorIs(t, err, ErrUnauthorized)
}

func TestNew_NeedsAProject(t *testing.T) {
	_, err := New(Config{})
	assert.Error(t, err)
}

func TestNew_EmptyTokenFileFailsTheCall(t *testing.T) {
	f := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(f, []byte("\n"), 0o600))
	c, err := New(Config{KeyFile: f, ProjectID: "p", BaseURL: "http://127.0.0.1:1"})
	require.NoError(t, err)
	_, err = c.GetElasticIP(t.Context(), "eip_000001")
	assert.ErrorContains(t, err, "empty")
}

// A write is one attempt: a throttle, a server failure or a lost connection after the
// caller's ownership check must not turn into a second PATCH.
func TestMove_PatchIsExactlyOneAttempt(t *testing.T) {
	for _, status := range []int{429, 500, 502, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			a, c := newClient(t, map[string][]reply{
				"GET /elastic_ips/eip_000001":   {{200, one("eip_000001", "203.0.113.1", "active", "sv_000001")}},
				"PATCH /elastic_ips/eip_000001": {{status, `{}`}},
			})
			err := c.MoveElasticIP(t.Context(), "eip_000001", "sv_000002")
			assert.ErrorIs(t, err, ErrUnavailable)
			assert.Len(t, a.bodies, 1, "exactly one PATCH")
		})
	}
}

func TestMove_PatchTransportFailureIsExactlyOneAttempt(t *testing.T) {
	var mu sync.Mutex
	patches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, one("eip_000001", "203.0.113.1", "active", "sv_000001"))
			return
		}
		mu.Lock()
		patches++
		mu.Unlock()
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(srv.Close)
	keyFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(keyFile, []byte(testToken), 0o600))
	c, err := New(Config{BaseURL: srv.URL, KeyFile: keyFile, ProjectID: "p", Backoff: time.Millisecond, PollInterval: time.Millisecond})
	require.NoError(t, err)

	err = c.MoveElasticIP(t.Context(), "eip_000001", "sv_000002")
	assert.ErrorIs(t, err, ErrUnavailable)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, patches)
}

func TestErrors_OnlyDocumentedCodesAreShown(t *testing.T) {
	_, c := newClient(t, map[string][]reply{"GET /elastic_ips/eip_000001": {{422, errBody(testToken)}}})
	_, err := c.GetElasticIP(t.Context(), "eip_000001")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testToken)
	assert.EqualError(t, err, "latitude: HTTP 422 Unprocessable Entity")
}

// A 200 can carry the token too, in a field the client reads.
func TestErrors_SuccessResponseFieldsNeverReachAnError(t *testing.T) {
	badAddr := `{"data":{"id":"eip_000001","attributes":{"address":"` + testToken + `","status":"active","mode":"routed","family":"IPv4","prefix_length":32}}}`
	_, c := newClient(t, map[string][]reply{
		"GET /elastic_ips/eip_000001": {{200, badAddr}},
	})
	_, err := c.GetElasticIP(t.Context(), "eip_000001")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testToken)
}

func TestErrors_ResponseEnumsAreShownOnlyWhenDocumented(t *testing.T) {
	odd := func(status, mode string) string {
		return `{"data":{"id":"eip_000001","attributes":{"address":"203.0.113.1","status":"` + status + `","mode":"` + mode + `","family":"` + testToken + `","prefix_length":32,"server":null}}}`
	}
	_, c := newClient(t, map[string][]reply{"GET /elastic_ips/eip_000001": {{200, odd("active", testToken)}}})
	err := c.MoveElasticIP(t.Context(), "eip_000001", "sv_000002")
	require.ErrorIs(t, err, ErrMoveRefused)
	assert.NotContains(t, err.Error(), testToken)

	_, c = newClient(t, map[string][]reply{"GET /elastic_ips/eip_000001": {{200, `{"data":{"id":"eip_000001","attributes":{"address":"203.0.113.1","status":"` + testToken + `","mode":"routed","family":"IPv4","prefix_length":32}}}`}}})
	err = c.MoveElasticIP(t.Context(), "eip_000001", "sv_000002")
	require.ErrorIs(t, err, ErrNotActive)
	assert.NotContains(t, err.Error(), testToken)
}

// A 307 or 308 would replay the PATCH body to wherever it points: a second write.
func TestMove_RedirectsAreNotFollowed(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var mu sync.Mutex
			patches := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, one("eip_000001", "203.0.113.1", "active", "sv_000001"))
					return
				}
				mu.Lock()
				patches++
				mu.Unlock()
				w.Header().Set("Location", r.URL.Path)
				w.WriteHeader(status)
			}))
			t.Cleanup(srv.Close)
			keyFile := filepath.Join(t.TempDir(), "token")
			require.NoError(t, os.WriteFile(keyFile, []byte(testToken), 0o600))
			c, err := New(Config{BaseURL: srv.URL, KeyFile: keyFile, ProjectID: "p", HTTPClient: srv.Client()})
			require.NoError(t, err)

			require.Error(t, c.MoveElasticIP(t.Context(), "eip_000001", "sv_000002"))
			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, 1, patches)
		})
	}
}

// IDs are opaque: whatever the API sends is returned unchanged, so equality against a
// configured ID stays exact. Short IDs and ones that look like the token are not rewritten.
func TestIDs_ReadFromAResponseAreReturnedUnchanged(t *testing.T) {
	for _, holder := range []string{"sv_a", testToken, "not an id"} {
		odd := `{"data":{"id":"eip_1","attributes":{"address":"203.0.113.1","status":"active","mode":"routed","family":"IPv4","prefix_length":32,"server":{"id":"` + holder + `"}}}}`
		_, c := newClient(t, map[string][]reply{"GET /elastic_ips/eip_1": {{200, odd}}})
		ip, err := c.GetElasticIP(t.Context(), "eip_1")
		require.NoError(t, err)
		assert.Equal(t, holder, ip.ServerID)
		assert.Equal(t, "eip_1", ip.ID)
	}
}
