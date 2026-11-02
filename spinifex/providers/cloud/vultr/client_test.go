package vultr

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reply is one canned response, served in order for a method and path.
type reply struct {
	status  int
	fixture string // testdata file name; empty for no body
}

type fakeAPI struct {
	t       *testing.T
	mu      sync.Mutex
	replies map[string][]reply
	seen    []string
	bodies  []map[string]string
	auth    []string
}

func newFakeAPI(t *testing.T, replies map[string][]reply) (*fakeAPI, *Client) {
	t.Helper()
	f := &fakeAPI{t: t, replies: replies}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	keyFile := filepath.Join(t.TempDir(), "api-key")
	require.NoError(t, os.WriteFile(keyFile, []byte("test-key\n"), 0o600))
	c, err := New(Config{BaseURL: srv.URL, KeyFile: keyFile, Region: "ord", Backoff: time.Millisecond})
	require.NoError(t, err)
	return f, c
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.seen = append(f.seen, key)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		var body map[string]string
		_ = json.Unmarshal(raw, &body)
		f.bodies = append(f.bodies, body)
	}
	queue := f.replies[key]
	if len(queue) == 0 {
		f.t.Errorf("unexpected call %s", key)
		w.WriteHeader(http.StatusTeapot)
		return
	}
	next := queue[0]
	f.replies[key] = queue[1:]
	if next.fixture == "" {
		w.WriteHeader(next.status)
		return
	}
	raw, err := os.ReadFile(filepath.Join("testdata", next.fixture))
	if err != nil {
		f.t.Errorf("fixture %s: %v", next.fixture, err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(next.status)
	_, _ = w.Write(raw)
}

const capturedID = "309a6acd-2042-4b31-883b-fbee5b64272a"
const capturedInstance = "e7d5eb7e-ca76-443c-8a59-fc450b1ed475"

func TestCreateReservedIP_SendsRegionAndLabel(t *testing.T) {
	f, c := newFakeAPI(t, map[string][]reply{
		"POST /reserved-ips": {{http.StatusCreated, "create.json"}},
	})
	ip, err := c.CreateReservedIP(context.Background(), "spinifex-capture:eipalloc-test")
	require.NoError(t, err)

	assert.Equal(t, capturedID, ip.ID)
	assert.Equal(t, "45.76.17.211", ip.Address.String())
	assert.Empty(t, ip.InstanceID)
	assert.Equal(t, map[string]string{"region": "ord", "ip_type": "v4", "label": "spinifex-capture:eipalloc-test"}, f.bodies[0])
	assert.Equal(t, "Bearer test-key", f.auth[0], "the key file is trimmed of its newline")
}

func TestCreateReservedIP_ServerErrorIsNotRetried(t *testing.T) {
	f, c := newFakeAPI(t, map[string][]reply{
		"POST /reserved-ips": {{http.StatusBadGateway, ""}},
	})
	_, err := c.CreateReservedIP(context.Background(), "x")
	require.ErrorIs(t, err, ErrUnavailable)
	assert.Len(t, f.seen, 1, "a create that may have succeeded must not be repeated")
}

func TestCreateReservedIP_ThrottleIsRetried(t *testing.T) {
	f, c := newFakeAPI(t, map[string][]reply{
		"POST /reserved-ips": {{http.StatusTooManyRequests, ""}, {http.StatusCreated, "create.json"}},
	})
	_, err := c.CreateReservedIP(context.Background(), "x")
	require.NoError(t, err)
	assert.Len(t, f.seen, 2)
}

func TestGetReservedIP_ReportsHolder(t *testing.T) {
	_, c := newFakeAPI(t, map[string][]reply{
		"GET /reserved-ips/" + capturedID: {{http.StatusOK, "get_attached.json"}},
	})
	ip, err := c.GetReservedIP(context.Background(), capturedID)
	require.NoError(t, err)
	assert.Equal(t, capturedInstance, ip.InstanceID)
}

func TestGetReservedIP_ServerErrorRetriesThenSucceeds(t *testing.T) {
	f, c := newFakeAPI(t, map[string][]reply{
		"GET /reserved-ips/" + capturedID: {{http.StatusServiceUnavailable, ""}, {http.StatusOK, "get.json"}},
	})
	_, err := c.GetReservedIP(context.Background(), capturedID)
	require.NoError(t, err)
	assert.Len(t, f.seen, 2)
}

func TestListReservedIPs_FiltersOtherRegions(t *testing.T) {
	_, c := newFakeAPI(t, map[string][]reply{
		"GET /reserved-ips": {{http.StatusOK, "list.json"}},
	})
	ips, err := c.ListReservedIPs(context.Background())
	require.NoError(t, err)
	require.Len(t, ips, 1)

	_, c2 := newFakeAPI(t, map[string][]reply{
		"GET /reserved-ips": {{http.StatusOK, "list.json"}},
	})
	c2.cfg.Region = "ewr"
	ips, err = c2.ListReservedIPs(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ips, "an address in another region is not this pool's")
}

func TestAttachReservedIP_AlreadyAttachedHereIsSuccess(t *testing.T) {
	_, c := newFakeAPI(t, map[string][]reply{
		"POST /reserved-ips/" + capturedID + "/attach": {{http.StatusBadRequest, "attach_already_attached.json"}},
		"GET /reserved-ips/" + capturedID:              {{http.StatusOK, "get_attached.json"}},
	})
	require.NoError(t, c.AttachReservedIP(context.Background(), capturedID, capturedInstance))
}

func TestAttachReservedIP_AttachedElsewhereIsHolderBusy(t *testing.T) {
	_, c := newFakeAPI(t, map[string][]reply{
		"POST /reserved-ips/" + capturedID + "/attach": {{http.StatusBadRequest, "attach_already_attached.json"}},
		"GET /reserved-ips/" + capturedID:              {{http.StatusOK, "get_attached.json"}},
	})
	err := c.AttachReservedIP(context.Background(), capturedID, "another-instance")
	require.ErrorIs(t, err, ErrHolderBusy)
}

func TestClassify_CapturedResponses(t *testing.T) {
	cases := []struct {
		fixture string
		status  int
		want    error
	}{
		{"get_missing.json", http.StatusNotFound, ErrNotFound},
		{"delete_missing.json", http.StatusNotFound, ErrNotFound},
		{"attach_missing_instance.json", http.StatusNotFound, ErrNotFound},
		{"unauthorized.json", http.StatusUnauthorized, ErrUnauthorized},
		{"attach_already_attached.json", http.StatusBadRequest, errAlreadyAttached},
		{"delete_attached_refused.json", http.StatusBadRequest, errDetachRefused},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", tc.fixture))
			require.NoError(t, err)
			err = classify(tc.status, raw)
			require.ErrorIs(t, err, tc.want)
			var apiErr *APIError
			require.ErrorAs(t, err, &apiErr)
			assert.NotEmpty(t, apiErr.Message)
		})
	}
}

func TestClassify_IPAllowlistRefusalIsUnauthorized(t *testing.T) {
	err := classify(http.StatusUnauthorized, []byte(`{"error":"Unauthorized IP address: 107.191.49.7","status":401}`))
	require.ErrorIs(t, err, ErrUnauthorized)
	assert.Contains(t, err.Error(), "Unauthorized IP address", "the operator needs Vultr's reason to fix the allowlist")
}

func TestClassify_BadRegionIsNotQuota(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "create_bad_region.json"))
	require.NoError(t, err)
	err = classify(http.StatusBadRequest, raw)
	assert.NotErrorIs(t, err, ErrQuotaExceeded)
	assert.NotErrorIs(t, err, ErrUnavailable)
}

func TestListInstanceVPCs(t *testing.T) {
	_, c := newFakeAPI(t, map[string][]reply{
		"GET /instances/" + capturedInstance + "/vpcs": {{http.StatusOK, "vpcs.json"}},
	})
	vpcs, err := c.ListInstanceVPCs(context.Background(), capturedInstance)
	require.NoError(t, err)
	require.Len(t, vpcs, 1)
	assert.Equal(t, "10.10.8.3", vpcs[0].Address.String())
	assert.Equal(t, "5a:00:06:f6:13:d0", vpcs[0].MAC)
}

func TestKeyRereadOnEveryCall(t *testing.T) {
	f, c := newFakeAPI(t, map[string][]reply{
		"POST /reserved-ips/" + capturedID + "/detach": {{http.StatusNoContent, ""}, {http.StatusNoContent, ""}},
	})
	require.NoError(t, c.DetachReservedIP(context.Background(), capturedID))
	require.NoError(t, os.WriteFile(c.cfg.KeyFile, []byte("rotated"), 0o600))
	require.NoError(t, c.DetachReservedIP(context.Background(), capturedID))
	assert.Equal(t, []string{"Bearer test-key", "Bearer rotated"}, f.auth)
}

func TestEmptyKeyFileFailsBeforeCalling(t *testing.T) {
	f, c := newFakeAPI(t, map[string][]reply{})
	require.NoError(t, os.WriteFile(c.cfg.KeyFile, []byte("\n"), 0o600))
	err := c.DeleteReservedIP(context.Background(), capturedID)
	require.Error(t, err)
	assert.Empty(t, f.seen)
}

// Vultr has refused a detach with "Unable to detach subnet" and accepted the
// same detach moments later, so the refusal is retried rather than surfaced.
func TestDetachAndDeleteRetryARefusedDetach(t *testing.T) {
	f, c := newFakeAPI(t, map[string][]reply{
		"POST /reserved-ips/" + capturedID + "/detach": {{http.StatusBadRequest, "delete_attached_refused.json"}, {http.StatusNoContent, ""}},
		"DELETE /reserved-ips/" + capturedID:           {{http.StatusBadRequest, "delete_attached_refused.json"}, {http.StatusNoContent, ""}},
	})
	require.NoError(t, c.DetachReservedIP(context.Background(), capturedID))
	require.NoError(t, c.DeleteReservedIP(context.Background(), capturedID))
	assert.Len(t, f.seen, 4)
}
