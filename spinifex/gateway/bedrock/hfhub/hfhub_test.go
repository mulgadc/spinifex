package hfhub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestClient builds a Client pointed at srv, with token attached to every
// request so gating tests can assert it was actually sent.
func newTestClient(srv *httptest.Server, token string) *Client {
	return &Client{BaseURL: srv.URL, Token: token, HTTP: srv.Client()}
}

// TestResolveRevision_PinsToSHA covers the ref-to-immutable-SHA pin: a
// mutable ref like "main" must resolve to the fixed commit sha the fake
// server reports, which is what a pull then downloads from.
func TestResolveRevision_PinsToSHA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/models/meta-llama/Llama-3.2-1B-Instruct/revision/main", r.URL.Path)
		_ = json.NewEncoder(w).Encode(revisionInfo{SHA: "abc123def456"})
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	sha, err := c.ResolveRevision(context.Background(), "meta-llama/Llama-3.2-1B-Instruct", "main")
	require.NoError(t, err)
	assert.Equal(t, "abc123def456", sha)
}

// TestResolveRevision_SendsBearerToken confirms a non-empty token is sent as
// an Authorization header, so a gated repo with a valid token is served.
func TestResolveRevision_SendsBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(revisionInfo{SHA: "sha1"})
	}))
	defer srv.Close()

	c := newTestClient(srv, "hf_secrettoken")
	_, err := c.ResolveRevision(context.Background(), "meta-llama/Llama-3.2-1B-Instruct", "main")
	require.NoError(t, err)
	assert.Equal(t, "Bearer hf_secrettoken", gotAuth)
}

// TestResolveRevision_GatedRepoReturnsAccessDeniedError covers D5: a 401/403
// from the hub must surface as a clean, awserrors-coded licence/credential
// error naming the repo, before any download is attempted.
func TestResolveRevision_GatedRepoReturnsAccessDeniedError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"gated"}`))
		}))

		c := newTestClient(srv, "")
		_, err := c.ResolveRevision(context.Background(), "meta-llama/Llama-3.2-1B-Instruct", "main")
		require.Error(t, err)
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorAccessDeniedException), "status %d: got %v", status, err)
		assert.Contains(t, err.Error(), "meta-llama/Llama-3.2-1B-Instruct")
		srv.Close()
	}
}

// TestResolveRevision_UnknownRepoReturnsNotFoundError covers a 404 mapping
// to a not-found error distinguishable from the gated/licence case.
func TestResolveRevision_UnknownRepoReturnsNotFoundError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	_, err := c.ResolveRevision(context.Background(), "nope/does-not-exist", "main")
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))
	assert.False(t, awserrors.IsErrorCode(err, awserrors.ErrorAccessDeniedException))
}

// TestListTree_ReturnsEntries covers the recursive tree listing pull filters
// down to the safetensors-only set.
func TestListTree_ReturnsEntries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/models/meta-llama/Llama-3.2-1B-Instruct/tree/abc123", r.URL.Path)
		assert.Equal(t, "true", r.URL.Query().Get("recursive"))
		_ = json.NewEncoder(w).Encode([]TreeEntry{
			{Type: "file", Path: "config.json", Size: 10, OID: "oid1"},
			{Type: "file", Path: "model.safetensors", Size: 1000, OID: "oid2"},
			{Type: "file", Path: "pytorch_model.bin", Size: 1000, OID: "oid3"},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	entries, err := c.ListTree(context.Background(), "meta-llama/Llama-3.2-1B-Instruct", "abc123")
	require.NoError(t, err)
	require.Len(t, entries, 3)
	assert.Equal(t, "model.safetensors", entries[1].Path)
}

// TestDownloadFile_StreamsBody covers the resolve URL shape and that the
// full body is readable from the returned reader.
func TestDownloadFile_StreamsBody(t *testing.T) {
	const content = "fake safetensors bytes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/meta-llama/Llama-3.2-1B-Instruct/resolve/abc123/model.safetensors", r.URL.Path)
		_, _ = io.WriteString(w, content)
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	body, _, err := c.DownloadFile(context.Background(), "meta-llama/Llama-3.2-1B-Instruct", "abc123", "model.safetensors")
	require.NoError(t, err)
	defer body.Close()

	got, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, content, string(got))
}

// TestDownloadFile_GatedReturnsAccessDeniedError covers a mid-stream-request
// gate (e.g. a token that expired between resolve and download) still
// mapping cleanly rather than surfacing a raw HTTP error.
func TestDownloadFile_GatedReturnsAccessDeniedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	body, _, err := c.DownloadFile(context.Background(), "meta-llama/Llama-3.2-1B-Instruct", "abc123", "model.safetensors")
	require.Error(t, err)
	assert.Nil(t, body)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorAccessDeniedException))
}

// TestNewClient_DefaultsBaseURLAndToken covers the constructor's defaults so
// a caller who passes an empty token still gets a usable client against the
// real hub.
func TestNewClient_DefaultsBaseURLAndToken(t *testing.T) {
	c := NewClient("")
	assert.Equal(t, DefaultBaseURL, c.BaseURL)
	assert.Empty(t, c.Token)
	assert.NotNil(t, c.HTTP)
}
