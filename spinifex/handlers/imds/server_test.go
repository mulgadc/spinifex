package handlers_imds

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loopbackListen returns a listenFunc backed by a real 127.0.0.1 listener so
// bind-manager logic runs end-to-end (including http.Server.Serve and the
// per-listener BaseContext) without root or the link-local address. It records
// the bound address per host-end so the test can dial it.
func loopbackListen(addrs *sync.Map) listenFunc {
	return func(ctx context.Context, hostEnd string) (net.Listener, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		addrs.Store(hostEnd, ln.Addr().String())
		return ln, nil
	}
}

func TestBindManager_BindServesWithVPCContext(t *testing.T) {
	const hostEnd = "imds-h-abc12345"
	var ensured, removed atomic.Int32
	ensure := func(_ context.Context, _ string) (string, error) {
		ensured.Add(1)
		return hostEnd, nil
	}
	remove := func(_ context.Context, _ string) error {
		removed.Add(1)
		return nil
	}

	// Echo the VPC ID the bind manager threaded into the request context — this
	// is the (VPC-ID, source-IP) → identity path the security model relies on.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vpcID, _ := r.Context().Value(ctxKeyVPCID).(string)
		_, _ = io.WriteString(w, vpcID)
	})

	var addrs sync.Map
	bm := newBindManager(nil, handler, ensure, remove, loopbackListen(&addrs))
	ctx := context.Background()

	require.NoError(t, bm.bind(ctx, "vpc-abc12345"))
	assert.Equal(t, int32(1), ensured.Load())

	// Second bind of the same VPC is a no-op (idempotent), no extra veth ensure.
	require.NoError(t, bm.bind(ctx, "vpc-abc12345"))
	assert.Equal(t, int32(1), ensured.Load())

	raw, ok := addrs.Load(hostEnd)
	require.True(t, ok)
	addr, ok := raw.(string)
	require.True(t, ok)

	resp, err := http.Get("http://" + addr + prefixMetaData)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, "vpc-abc12345", string(body), "handler must see the listener's VPC ID")

	// Unbind closes the listener and tears down the veth.
	bm.unbind(ctx, "vpc-abc12345")
	assert.Equal(t, int32(1), removed.Load())

	client := http.Client{Timeout: 500 * time.Millisecond}
	_, err = client.Get("http://" + addr + prefixMetaData)
	assert.Error(t, err, "listener must be closed after unbind")
}

func TestBindManager_BindPropagatesVethError(t *testing.T) {
	ensure := func(_ context.Context, _ string) (string, error) {
		return "", errEnsureFailed
	}
	bm := newBindManager(nil, http.NewServeMux(), ensure, func(context.Context, string) error { return nil }, loopbackListen(&sync.Map{}))
	err := bm.bind(context.Background(), "vpc-x")
	require.Error(t, err)
}

var errEnsureFailed = io.ErrUnexpectedEOF
