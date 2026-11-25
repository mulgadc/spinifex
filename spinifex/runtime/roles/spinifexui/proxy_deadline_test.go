package spinifexui

import (
	"bufio"
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

// readTimeout is the console's ReadTimeout and WriteTimeout in these tests.
const readTimeout = 100 * time.Millisecond

// slowUpload PUTs a body that stalls past readTimeout after its first part, as
// an object part does over a slow link.
func slowUpload(t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	body, writer := io.Pipe()
	go func() {
		if _, err := writer.Write([]byte(strings.Repeat("x", 1024))); err != nil {
			return
		}
		time.Sleep(readTimeout * 5 / 2)
		if _, err := writer.Write([]byte(strings.Repeat("x", 4*1024))); err != nil {
			return
		}
		_ = writer.Close()
	}()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url, body)
	require.NoError(t, err)
	req.ContentLength = 5 * 1024
	return http.DefaultClient.Do(req)
}

// newDeadlineFront starts the console handler with short server timeouts, in
// front of a backend that answers with the number of body bytes it received.
func newDeadlineFront(t *testing.T) string {
	t.Helper()
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
	front.Config.ReadTimeout = readTimeout
	front.Config.WriteTimeout = readTimeout
	front.Start()
	t.Cleanup(front.Close)
	return front.URL
}

func TestHandler_S3ProxyOutlastsTheServerReadTimeout(t *testing.T) {
	t.Parallel()

	t.Run("s3 route extends the deadline", func(t *testing.T) {
		t.Parallel()
		resp, err := slowUpload(t, newDeadlineFront(t)+"/proxy/s3/bucket/key")
		require.NoError(t, err)
		defer resp.Body.Close()
		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "5120", string(got), "the backend received the whole body")
	})

	// The gateway route keeps the server's timeout, so the same body fails there.
	t.Run("gateway route keeps the server timeout", func(t *testing.T) {
		t.Parallel()
		resp, err := slowUpload(t, newDeadlineFront(t)+"/proxy/awsgw/")
		if err == nil {
			defer resp.Body.Close()
			assert.NotEqual(t, http.StatusOK, resp.StatusCode)
		}
	})
}

func TestHandler_AWSGWProxyPassesAnEventStreamThroughAsItArrives(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: sample\ndata: {}\n\n"))
		http.NewResponseController(w).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(backend.Close)
	t.Cleanup(func() { close(release) })

	transport := backend.Client().Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, backend.Listener.Addr().String())
	}
	transport.TLSClientConfig.ServerName = "example.com"

	contentFS, err := fs.Sub(distFS, "frontend/dist")
	require.NoError(t, err)
	svc := &Service{Config: &Config{Region: "us-west-1"}}
	front := httptest.NewServer(svc.newHandler(contentFS, filepath.Join(t.TempDir(), "ca.pem"), transport))
	t.Cleanup(front.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, front.URL+"/proxy/awsgw/", strings.NewReader("Action=GetInstanceMetrics"))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// The backend has not finished, so this line can only come from a flush.
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "event: sample\n", line)
}
