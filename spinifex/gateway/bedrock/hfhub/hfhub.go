// Package hfhub is a minimal Hugging Face Hub client: resolve a repo
// revision to an immutable commit SHA, list its file tree, and stream
// individual files. It is the network half of 'ochre weights pull'; it never
// writes to predastore or touches the weights KV store itself.
package hfhub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// DefaultBaseURL is the public Hugging Face Hub API and file host.
const DefaultBaseURL = "https://huggingface.co"

// defaultTimeout bounds a single HTTP call (metadata lookups and the
// per-chunk read deadline implied by http.Client), not a whole file
// download, which can run for as long as the server keeps streaming.
const defaultTimeout = 30 * time.Second

// Client resolves a Hugging Face repo revision to an immutable commit SHA,
// lists its file tree, and streams individual files. BaseURL is overridable
// so tests run against an httptest fake instead of the real hub.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewClient builds a Client against the public Hugging Face hub. token may
// be empty for public repos; a gated or private repo without one fails with
// a licence/credential error from ResolveRevision or ListTree.
func NewClient(token string) *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		Token:   token,
		HTTP:    &http.Client{Timeout: defaultTimeout},
	}
}

// TreeEntry is one file or directory in a Hugging Face repo's tree listing.
type TreeEntry struct {
	Type string `json:"type"` // "file" or "directory"
	Path string `json:"path"`
	Size int64  `json:"size"`
	OID  string `json:"oid"`
}

// revisionInfo is the subset of the Hugging Face model-info response pull
// needs: the immutable commit SHA a mutable ref (branch/tag) resolves to.
type revisionInfo struct {
	SHA string `json:"sha"`
}

// ResolveRevision resolves repo's revision (branch, tag, or SHA; e.g. "main")
// to its immutable commit SHA, so a pull is pinned to weights that cannot
// silently change under an already-served model ID.
func (c *Client) ResolveRevision(ctx context.Context, repo, revision string) (string, error) {
	url := fmt.Sprintf("%s/api/models/%s/revision/%s", c.baseURL(), repo, revision)
	body, err := c.getJSON(ctx, repo, url)
	if err != nil {
		return "", err
	}
	defer body.Close()

	var info revisionInfo
	if err := json.NewDecoder(body).Decode(&info); err != nil {
		return "", fmt.Errorf("decode revision info for %s@%s: %w", repo, revision, err)
	}
	if info.SHA == "" {
		return "", fmt.Errorf("hugging face returned no commit sha for %s@%s", repo, revision)
	}
	return info.SHA, nil
}

// ListTree lists every file and directory in repo at commit sha, recursively.
func (c *Client) ListTree(ctx context.Context, repo, sha string) ([]TreeEntry, error) {
	url := fmt.Sprintf("%s/api/models/%s/tree/%s?recursive=true", c.baseURL(), repo, sha)
	body, err := c.getJSON(ctx, repo, url)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	var entries []TreeEntry
	if err := json.NewDecoder(body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode file tree for %s@%s: %w", repo, sha, err)
	}
	return entries, nil
}

// DownloadFile streams path from repo at commit sha. The caller must close
// the returned reader. contentLength is -1 when the server does not report
// it (http.Response.ContentLength's own convention).
func (c *Client) DownloadFile(ctx context.Context, repo, sha, path string) (io.ReadCloser, int64, error) {
	url := fmt.Sprintf("%s/%s/resolve/%s/%s", c.baseURL(), repo, sha, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build request for %s: %w", path, err)
	}
	c.authorize(req)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("download %s from %s: %w", path, repo, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, 0, mapStatusError(repo, resp)
	}
	return resp.Body, resp.ContentLength, nil
}

// getJSON issues an authenticated GET against url and returns the response
// body on 2xx; the caller decodes and closes it. Non-2xx is mapped to a
// clear licence/not-found/generic error naming repo.
func (c *Client) getJSON(ctx context.Context, repo, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	c.authorize(req)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", repo, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, mapStatusError(repo, resp)
	}
	return resp.Body, nil
}

func (c *Client) authorize(req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return DefaultBaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: defaultTimeout}
}

// mapStatusError maps a non-2xx Hugging Face response to a clear
// awserrors-coded error naming repo: 401/403 as a licence/credential
// failure (D5 -- pull must abort before any object lands), 404 as
// not-found, anything else as a generic upstream error.
func mapStatusError(repo string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return awserrors.Errorf(awserrors.ErrorAccessDeniedException,
			"hugging face repo %q requires a licence/credential (HTTP %d): %s", repo, resp.StatusCode, string(body))
	case http.StatusNotFound:
		return awserrors.Errorf(awserrors.ErrorResourceNotFoundException,
			"hugging face repo %q not found (HTTP %d): %s", repo, resp.StatusCode, string(body))
	default:
		return fmt.Errorf("hugging face request for %q failed (HTTP %d): %s", repo, resp.StatusCode, string(body))
	}
}
