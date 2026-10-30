package spinifexui

import (
	"context"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slowUpload PUTs a body that takes longer to arrive than the console's
// ReadTimeout, as an object part does over a slow link.
func slowUpload(t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	body, writer := io.Pipe()
	go func() {
		for range 5 {
			time.Sleep(80 * time.Millisecond)
			if _, err := writer.Write([]byte(strings.Repeat("x", 1024))); err != nil {
				return
			}
		}
		_ = writer.Close()
	}()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url, body)
	require.NoError(t, err)
	req.ContentLength = 5 * 1024
	return http.DefaultClient.Do(req)
}

func TestHandler_S3ProxyOutlastsTheServerReadTimeout(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(strconv.FormatInt(n, 10)))
	}))
	t.Cleanup(backend.Close)

	// Every backend host the proxy names resolves to the test backend.
	transport := backend.Client().Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, backend.Listener.Addr().String())
	}
	transport.TLSClientConfig.ServerName = "example.com"

	contentFS, err := fs.Sub(distFS, "frontend/dist")
	require.NoError(t, err)
	svc := &Service{Config: &Config{Region: "us-west-1"}}
	front := httptest.NewUnstartedServer(svc.newHandler(contentFS, filepath.Join(t.TempDir(), "ca.pem"), transport))
	front.Config.ReadTimeout = 150 * time.Millisecond
	front.Config.WriteTimeout = 150 * time.Millisecond
	front.Start()
	t.Cleanup(front.Close)

	resp, err := slowUpload(t, front.URL+"/proxy/s3/bucket/key")
	require.NoError(t, err)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "5120", string(got), "the backend received the whole body")

	// The gateway route keeps the server's timeout, so the same body fails there.
	resp, err = slowUpload(t, front.URL+"/proxy/awsgw/")
	if err == nil {
		defer resp.Body.Close()
		assert.NotEqual(t, http.StatusOK, resp.StatusCode)
	}
}
