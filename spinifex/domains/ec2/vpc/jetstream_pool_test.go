package vpc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// pooledJetStream is an embedded JetStream server that one test holds at a
// time. Between tests every stream is emptied or deleted, so each test sees
// the same state a fresh server plus the service's own buckets would give.
type pooledJetStream struct {
	ns *server.Server
	// buckets holds the config of each stream the service created on its
	// first start. Only these streams are kept between tests.
	buckets map[string]jetstream.StreamConfig
}

// jsPool outlives every test, so its stores live under root, which TestMain
// creates and removes, rather than under a per-test temp dir.
var jsPool struct {
	mu   sync.Mutex
	root string
	dirs int
	free []*pooledJetStream
	all  []*pooledJetStream
}

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "vpc-js-pool-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create JetStream pool dir:", err)
		os.Exit(1)
	}
	jsPool.root = root
	code := m.Run()
	jsPool.mu.Lock()
	for _, p := range jsPool.all {
		p.ns.Shutdown()
		p.ns.WaitForShutdown()
	}
	jsPool.mu.Unlock()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

func newPooledJetStream(t *testing.T) *pooledJetStream {
	t.Helper()
	jsPool.mu.Lock()
	jsPool.dirs++
	dir := filepath.Join(jsPool.root, strconv.Itoa(jsPool.dirs))
	jsPool.mu.Unlock()
	require.NoError(t, os.Mkdir(dir, 0o700))
	ns, err := server.NewServer(&server.Options{
		DontListen: true,
		JetStream:  true,
		StoreDir:   dir,
		NoLog:      true,
		NoSigs:     true,
	})
	require.NoError(t, err)
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("embedded NATS server not ready for connections within 5s")
	}
	p := &pooledJetStream{ns: ns}
	jsPool.mu.Lock()
	jsPool.all = append(jsPool.all, p)
	jsPool.mu.Unlock()
	return p
}

// acquireJetStream gives the test sole use of a pooled server and a fresh
// connection to it. The server goes back to the pool after every other
// cleanup of the test has run, so the test's connection is already closed.
func acquireJetStream(t *testing.T) (*pooledJetStream, *nats.Conn) {
	t.Helper()
	clustersize.DeclareForTest(t, 1)
	jsPool.mu.Lock()
	var p *pooledJetStream
	if n := len(jsPool.free); n > 0 {
		p = jsPool.free[n-1]
		jsPool.free = jsPool.free[:n-1]
	}
	jsPool.mu.Unlock()
	if p == nil {
		p = newPooledJetStream(t)
	}
	t.Cleanup(func() {
		if err := p.reset(); err != nil {
			t.Errorf("reset pooled JetStream server: %v", err)
			return
		}
		jsPool.mu.Lock()
		jsPool.free = append(jsPool.free, p)
		jsPool.mu.Unlock()
	})

	return p, p.connect(t)
}

// connect opens another in-process connection to the held server. It is
// closed when the test ends, before the server goes back to the pool.
func (p *pooledJetStream) connect(t *testing.T) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect("", nats.InProcessServer(p.ns))
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	return nc
}

// newPooledTestVPCService starts the service on a pooled server that holds
// only empty buckets, which is what a fresh server would give.
func newPooledTestVPCService(t *testing.T, cfg *config.Config) (*VPCServiceImpl, *nats.Conn) {
	t.Helper()
	p, nc := acquireJetStream(t)
	svc, err := NewVPCServiceImplWithNATS(t.Context(), cfg, nc)
	require.NoError(t, err)
	require.NoError(t, p.recordBuckets(t.Context(), testutil.NewJetStream(t, nc)))
	return svc, nc
}

// recordBuckets keeps the configs of the streams present right after the
// first service start on this server. Later starts find them and reuse them.
func (p *pooledJetStream) recordBuckets(ctx context.Context, js jetstream.JetStream) error {
	if p.buckets != nil {
		return nil
	}
	buckets := map[string]jetstream.StreamConfig{}
	lister := js.ListStreams(ctx)
	for info := range lister.Info() {
		buckets[info.Config.Name] = info.Config
	}
	if err := lister.Err(); err != nil {
		return err
	}
	p.buckets = buckets
	return nil
}

// reset deletes every consumer, purges each recorded bucket whose config is
// unchanged and deletes any other stream.
func (p *pooledJetStream) reset() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nc, err := nats.Connect("", nats.InProcessServer(p.ns))
	if err != nil {
		return err
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	lister := js.ListStreams(ctx)
	var infos []*jetstream.StreamInfo
	for info := range lister.Info() {
		infos = append(infos, info)
	}
	if err := lister.Err(); err != nil {
		return err
	}
	for _, info := range infos {
		name := info.Config.Name
		want, keep := p.buckets[name]
		if !keep || !reflect.DeepEqual(want, info.Config) {
			if err := js.DeleteStream(ctx, name); err != nil {
				return err
			}
			continue
		}
		if info.State.Consumers == 0 && info.State.Msgs == 0 {
			continue
		}
		s, err := js.Stream(ctx, name)
		if err != nil {
			return err
		}
		if info.State.Consumers > 0 {
			if err := deleteConsumers(ctx, s); err != nil {
				return err
			}
		}
		if info.State.Msgs > 0 {
			if err := s.Purge(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func deleteConsumers(ctx context.Context, s jetstream.Stream) error {
	lister := s.ConsumerNames(ctx)
	var names []string
	for c := range lister.Name() {
		names = append(names, c)
	}
	if err := lister.Err(); err != nil && !errors.Is(err, jetstream.ErrEndOfData) {
		return err
	}
	for _, c := range names {
		if err := s.DeleteConsumer(ctx, c); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
			return err
		}
	}
	return nil
}
