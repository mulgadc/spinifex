package handlers_imds

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loopbackListen returns a listenFunc backed by a real 127.0.0.1 listener so
// bind-manager logic runs end-to-end (including http.Server.Serve and the
// per-listener BaseContext) without root or the link-local address. It records
// the bound address per host-end so the test can dial it.
func loopbackListen(addrs *sync.Map) listenFunc {
	return func(ctx context.Context, netnsName, hostEnd string) (net.Listener, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		addrs.Store(hostEnd, ln.Addr().String())
		return ln, nil
	}
}

func TestBindManager_BindServesWithVPCContext(t *testing.T) {
	const (
		hostEnd  = "imds-h-abc12345"
		subnetID = "subnet-abc12345"
		vpcID    = "vpc-abc12345"
	)
	var ensured, removed atomic.Int32
	var ensuredKey, removedKey atomic.Value
	ensure := func(_ context.Context, key string, _ netip.Prefix) (string, string, error) {
		ensured.Add(1)
		ensuredKey.Store(key)
		return "", hostEnd, nil
	}
	remove := func(_ context.Context, key string) error {
		removed.Add(1)
		removedKey.Store(key)
		return nil
	}

	// Echo the VPC + subnet the bind manager threaded into the request context.
	// The VPC keys the eni-by-vpc-ip index — the (VPC-ID, source-IP) → identity
	// path the security model relies on; the subnet rides along for triage.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxVPC, _ := r.Context().Value(ctxKeyVPCID).(string)
		ctxSubnet, _ := r.Context().Value(ctxKeySubnetID).(string)
		_, _ = io.WriteString(w, ctxVPC+"|"+ctxSubnet)
	})

	var addrs sync.Map
	bm := newBindManager(nil, handler, ensure, remove, loopbackListen(&addrs))
	ctx := context.Background()
	cidr := netip.MustParsePrefix("10.211.0.0/16")

	require.NoError(t, bm.bind(ctx, subnetID, vpcID, cidr))
	assert.Equal(t, int32(1), ensured.Load())
	assert.Equal(t, subnetID, ensuredKey.Load(), "veth lifecycle is keyed by subnet")

	// Second bind of the same key is a no-op (idempotent), no extra veth ensure.
	require.NoError(t, bm.bind(ctx, subnetID, vpcID, cidr))
	assert.Equal(t, int32(1), ensured.Load())

	raw, ok := addrs.Load(hostEnd)
	require.True(t, ok)
	addr, ok := raw.(string)
	require.True(t, ok)

	resp, err := http.Get("http://" + addr + prefixMetaData)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, vpcID+"|"+subnetID, string(body), "handler must see the listener's VPC and subnet")

	// Unbind closes the listener and tears down the veth, keyed by subnet.
	bm.unbind(ctx, subnetID)
	assert.Equal(t, int32(1), removed.Load())
	assert.Equal(t, subnetID, removedKey.Load(), "veth removal is keyed by subnet")

	client := http.Client{Timeout: 500 * time.Millisecond}
	_, err = client.Get("http://" + addr + prefixMetaData)
	assert.Error(t, err, "listener must be closed after unbind")
}

func TestBindManager_BindPropagatesVethError(t *testing.T) {
	ensure := func(_ context.Context, _ string, _ netip.Prefix) (string, string, error) {
		return "", "", errEnsureFailed
	}
	bm := newBindManager(nil, http.NewServeMux(), ensure, func(context.Context, string) error { return nil }, loopbackListen(&sync.Map{}))
	err := bm.bind(context.Background(), "subnet-x", "vpc-x", netip.MustParsePrefix("10.211.0.0/16"))
	require.Error(t, err)
}

var errEnsureFailed = io.ErrUnexpectedEOF

// TestBindManager_SyncSkipsVersionKey guards that the schema-version marker
// migrate.RunKV stamps into the subnet-veth bucket is not mistaken for a subnet
// ID and made to bring up a bogus veth + listener.
func TestBindManager_SyncSkipsVersionKey(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	subnetVeth, _, err := InitBuckets(js, 1)
	require.NoError(t, err)

	// InitBuckets → migrate.RunKV stamps utils.VersionKey; add one real subnet
	// record (sync resolves its CIDR from the record before binding).
	require.NoError(t, NewVethStore(subnetVeth).Put(SubnetVethRecord{
		SubnetID:   "subnet-abcdef12",
		VPCID:      "vpc-abcdef12",
		SubnetCIDR: "10.211.0.0/16",
	}))

	var bound sync.Map
	ensure := func(_ context.Context, subnetID string, _ netip.Prefix) (string, string, error) {
		bound.Store(subnetID, true)
		return "imds-" + subnetID, "imds-h-" + subnetID, nil
	}
	remove := func(context.Context, string) error { return nil }
	bm := newBindManager(subnetVeth, http.NewServeMux(), ensure, remove, loopbackListen(&sync.Map{}))

	require.NoError(t, bm.sync(context.Background()))
	defer bm.shutdown()

	_, versionBound := bound.Load(utils.VersionKey)
	assert.False(t, versionBound, "_version marker must not be bound as a subnet")
	_, subnetBound := bound.Load("subnet-abcdef12")
	assert.True(t, subnetBound, "real subnet entry must be bound")
}
