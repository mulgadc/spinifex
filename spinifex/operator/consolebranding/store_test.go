package consolebranding_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/operator/consolebranding"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newStore(t *testing.T) consolebranding.Store {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := consolebranding.OpenBucket(t.Context(), js)
	require.NoError(t, err)
	return consolebranding.Store{KV: kv}
}

func TestStore_RevisionGuardsEveryWrite(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()

	rec, rev, err := s.Get(ctx)
	require.NoError(t, err)
	assert.Nil(t, rec)
	assert.Zero(t, rev)

	rev1, err := s.Put(ctx, consolebranding.Record{SchemaVersion: 1, ProductName: "Acme"}, 0)
	require.NoError(t, err)

	_, err = s.Put(ctx, consolebranding.Record{SchemaVersion: 1, ProductName: "Second create"}, 0)
	require.ErrorIs(t, err, consolebranding.ErrStaleRevision, "a create over an existing record is stale")

	rev2, err := s.Put(ctx, consolebranding.Record{SchemaVersion: 1, ProductName: "Acme Cloud"}, rev1)
	require.NoError(t, err)

	_, err = s.Put(ctx, consolebranding.Record{SchemaVersion: 1, ProductName: "Lost update"}, rev1)
	require.ErrorIs(t, err, consolebranding.ErrStaleRevision)

	rec, rev, err = s.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, rev2, rev)
	assert.Equal(t, "Acme Cloud", rec.ProductName, "a stale write leaves the record unchanged")
}

// failingKV stands in for a NATS connection that has gone away.
type failingKV struct{ consolebranding.KV }

func (failingKV) Get(context.Context, string) (jetstream.KeyValueEntry, error) {
	return nil, errors.New("nats: connection closed")
}

func TestCache_KeepsLastRecordWhenStoreFails(t *testing.T) {
	s := newStore(t)
	rev, err := s.Put(t.Context(), consolebranding.Record{SchemaVersion: 1, ProductName: "Acme"}, 0)
	require.NoError(t, err)

	var c consolebranding.Cache
	require.NoError(t, c.Refresh(t.Context(), s))

	require.Error(t, c.Refresh(t.Context(), consolebranding.Store{KV: failingKV{}}))
	rec, gotRev := c.Snapshot()
	require.NotNil(t, rec)
	assert.Equal(t, "Acme", rec.ProductName)
	assert.Equal(t, rev, gotRev)
}

func TestCache_IgnoresOlderRevision(t *testing.T) {
	var c consolebranding.Cache
	c.Set(&consolebranding.Record{ProductName: "new"}, 5)
	c.Set(&consolebranding.Record{ProductName: "old"}, 4)

	rec, rev := c.Snapshot()
	assert.Equal(t, "new", rec.ProductName)
	assert.Equal(t, uint64(5), rev)
}

func TestCache_RunPicksUpAPutWithoutARestart(t *testing.T) {
	s := newStore(t)
	var c consolebranding.Cache
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go c.Run(ctx, s, 20*time.Millisecond)

	rev, err := s.Put(t.Context(), consolebranding.Record{SchemaVersion: 1, ProductName: "Acme"}, 0)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, got := c.Snapshot()
		return got == rev
	}, 5*time.Second, 10*time.Millisecond)
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

func publicView(t *testing.T, w *httptest.ResponseRecorder) consolebranding.Public {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	var out consolebranding.Public
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	return out
}

func brandedCache(rev uint64) *consolebranding.Cache {
	c := &consolebranding.Cache{}
	c.Set(&consolebranding.Record{
		ProductName: "Acme Cloud",
		DocsURL:     ptr(""),
		Primary:     operatorv1.BrandingColors{Light: "#0A4A8F"},
		Logo:        &operatorv1.BrandingImage{ContentType: consolebranding.TypeSVG, Data: svgBytes},
		Favicon:     &operatorv1.BrandingImage{ContentType: consolebranding.TypePNG, Data: pngBytes},
	}, rev)
	return c
}

func TestHandler_GateOffServesTheBuiltInBrand(t *testing.T) {
	h := consolebranding.Handler{Enabled: false, Cache: brandedCache(7)}

	out := publicView(t, get(t, h, "/console/branding/branding.json"))
	assert.Equal(t, consolebranding.Public{Enabled: false}, out, "a stored record is kept but not served")
	assert.Equal(t, http.StatusNotFound, get(t, h, "/console/branding/logo?rev=7").Code)
}

func TestHandler_NoRecordServesTheBuiltInBrand(t *testing.T) {
	h := consolebranding.Handler{Enabled: true, Cache: &consolebranding.Cache{}}

	assert.Equal(t, consolebranding.Public{Enabled: true}, publicView(t, get(t, h, "/console/branding/branding.json")))
	assert.Equal(t, http.StatusNotFound, get(t, h, "/console/branding/branding.css?rev=0").Code)
}

func TestHandler_ServesVersionedAssets(t *testing.T) {
	h := consolebranding.Handler{Enabled: true, Cache: brandedCache(7)}

	out := publicView(t, get(t, h, "/console/branding/branding.json"))
	assert.Equal(t, consolebranding.Public{
		Enabled: true, Revision: 7, ProductName: "Acme Cloud", DocsURL: ptr(""),
		Stylesheet: "branding.css?rev=7", Logo: "logo?rev=7", Favicon: "favicon?rev=7",
	}, out)

	css := get(t, h, "/console/branding/branding.css?rev=7")
	require.Equal(t, http.StatusOK, css.Code)
	assert.Equal(t, "text/css; charset=utf-8", css.Header().Get("Content-Type"))
	assert.Contains(t, css.Header().Get("Cache-Control"), "immutable")
	assert.Equal(t, ":root:not(.dark){--primary:#0A4A8F;--ring:#0A4A8F}\n", css.Body.String())

	logo := get(t, h, "/console/branding/logo?rev=7")
	require.Equal(t, http.StatusOK, logo.Code)
	assert.Equal(t, consolebranding.TypeSVG, logo.Header().Get("Content-Type"))
	assert.Equal(t, "nosniff", logo.Header().Get("X-Content-Type-Options"))
	assert.Contains(t, logo.Header().Get("Content-Security-Policy"), "sandbox")
	assert.Equal(t, svgBytes, logo.Body.Bytes())

	fav := get(t, h, "/console/branding/favicon?rev=7")
	require.Equal(t, http.StatusOK, fav.Code)
	assert.Empty(t, fav.Header().Get("Content-Security-Policy"), "only SVG needs the sandbox")
	assert.Equal(t, "nosniff", fav.Header().Get("X-Content-Type-Options"))

	assert.Equal(t, http.StatusNotFound, get(t, h, "/console/branding/logo-dark?rev=7").Code, "an unset image is absent")
}

// A URL from an earlier revision must not serve today's bytes under a
// year-long cache header, or a reset would never reach a browser.
func TestHandler_StaleRevisionIsNotFound(t *testing.T) {
	h := consolebranding.Handler{Enabled: true, Cache: brandedCache(7)}

	for _, rev := range []string{"6", "8", "", "7x"} {
		w := get(t, h, "/console/branding/logo?rev="+rev)
		assert.Equal(t, http.StatusNotFound, w.Code, "rev=%s", rev)
		assert.Empty(t, w.Header().Get("Cache-Control"), "rev=%s", rev)
	}
}

func TestHandler_RefusesWrites(t *testing.T) {
	h := consolebranding.Handler{Enabled: true, Cache: brandedCache(7)}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/console/branding/branding.json", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// The public route reads only memory, so it answers at once while the store
// is unreachable.
func TestHandler_AnswersWhileStoreIsDown(t *testing.T) {
	c := brandedCache(3)
	require.Error(t, c.Refresh(t.Context(), consolebranding.Store{KV: failingKV{}}))
	h := consolebranding.Handler{Enabled: true, Cache: c}

	out := publicView(t, get(t, h, "/console/branding/branding.json"))
	assert.Equal(t, "Acme Cloud", out.ProductName)
	assert.Equal(t, "logo?rev="+strconv.Itoa(3), out.Logo)
}

// A bucket that cannot open at startup leaves the built-in brand in place and
// is opened again by a later refresh.
func TestLazyBucket_RetriesAFailedOpen(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	down := true
	opens := 0
	b := &consolebranding.LazyBucket{JS: js, Open: func(ctx context.Context, m jetstream.KeyValueManager) (jetstream.KeyValue, error) {
		opens++
		if down {
			return nil, errors.New("nats: no responders available for request")
		}
		return consolebranding.OpenBucket(ctx, m)
	}}
	s := consolebranding.Store{KV: b}

	var c consolebranding.Cache
	require.Error(t, c.Refresh(t.Context(), s))
	rec, rev := c.Snapshot()
	assert.Nil(t, rec, "a failed open leaves the built-in brand")
	assert.Zero(t, rev)

	down = false
	rev1, err := s.Put(t.Context(), consolebranding.Record{SchemaVersion: 1, ProductName: "Acme"}, 0)
	require.NoError(t, err)
	require.NoError(t, c.Refresh(t.Context(), s))
	rec, rev = c.Snapshot()
	require.NotNil(t, rec)
	assert.Equal(t, "Acme", rec.ProductName)
	assert.Equal(t, rev1, rev)
	assert.Equal(t, 2, opens, "an open bucket is kept, not opened on every call")
}
