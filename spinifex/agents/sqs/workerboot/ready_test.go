package workerboot

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nkeys"
)

// describer is the gateway's DescribeStore, answering from a script.
type describer struct {
	answers []describeAnswer
	calls   int
}

type describeAnswer struct {
	st  Store
	err error
}

func (d *describer) DescribeStore(context.Context) (Store, error) {
	d.calls++
	a := d.answers[min(d.calls, len(d.answers))-1]
	return a.st, a.err
}

type checker struct {
	errs  []error
	calls int
	seen  []string
}

func (c *checker) Check(_ context.Context, address, _ string, kp nkeys.KeyPair) error {
	c.calls++
	pub, _ := kp.PublicKey()
	c.seen = append(c.seen, address+"|"+pub)
	if len(c.errs) > 0 {
		err := c.errs[0]
		c.errs = c.errs[1:]
		return err
	}
	return nil
}

// bootedGuest has run the boot step, so its disk holds a recorded key.
func bootedGuest(t *testing.T) (*guest, string) {
	t.Helper()
	g := newGuest(t)
	g.contract()
	pub, err := g.boot()
	if err != nil {
		t.Fatal(err)
	}
	return g, pub
}

func (g *guest) readyCfg(d StoreDescriber, c Checker) ReadyConfig {
	return ReadyConfig{
		Config:       g.cfg,
		NewDescriber: func(string, string) (StoreDescriber, error) { return d, nil },
		Checker:      c,
	}
}

// hooked asks stop before each describe, after the previous pass has written
// or removed the marker, and ends the watch when stop says so.
type hooked struct {
	d      StoreDescriber
	marker string
	stop   func(call int, ready bool) bool
	cancel context.CancelFunc
	calls  int
}

func (h *hooked) DescribeStore(ctx context.Context) (Store, error) {
	h.calls++
	_, err := os.Stat(h.marker)
	if h.stop(h.calls, err == nil) {
		h.cancel()
		return Store{}, context.Canceled
	}
	return h.d.DescribeStore(ctx)
}

// watch runs Watch with millisecond cadences until stop ends it, then checks
// the marker was removed on the way out.
func (g *guest) watch(cfg ReadyConfig, stop func(call int, ready bool) bool) error {
	g.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	inner := cfg.NewDescriber
	cfg.NewDescriber = func(region, url string) (StoreDescriber, error) {
		d, err := inner(region, url)
		if err != nil {
			return nil, err
		}
		return &hooked{d: d, marker: filepath.Join(cfg.RunDir, readyFile), stop: stop, cancel: cancel}, nil
	}
	for _, p := range []*time.Duration{&cfg.Wait, &cfg.Refresh, &cfg.Refused} {
		if *p == 0 {
			*p = time.Millisecond
		}
	}
	err := Watch(ctx, cfg)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		g.t.Fatal("the watch never reached its stop condition")
	}
	g.assertNotReady()
	return err
}

func bound(pub string) describeAnswer {
	return describeAnswer{st: Store{Bound: true, Address: "10.240.0.9", PublicKey: pub}}
}

func (g *guest) marker() string {
	g.t.Helper()
	b, _ := os.ReadFile(filepath.Join(g.cfg.RunDir, readyFile))
	return string(b)
}

// A store launched long after the worker booted, far past the old 15-minute
// bound of 180 attempts, still becomes ready; nothing is published before.
func TestWatch_StoreLaunchedLateBecomesReady(t *testing.T) {
	g, pub := bootedGuest(t)
	answers := make([]describeAnswer, 201)
	answers[200] = bound(pub)
	d := &describer{answers: answers}
	c := &checker{errs: []error{&StoreUnreachableError{Err: errors.New("connection refused")}}}
	published := ""
	err := g.watch(g.readyCfg(d, c), func(call int, ready bool) bool {
		if ready {
			published = g.marker()
			return true
		}
		if call > 202 {
			t.Fatalf("not ready by call %d", call)
		}
		return false
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("watch = %v", err)
	}
	if d.calls != 202 || c.calls != 2 || c.seen[0] != "10.240.0.9|"+pub {
		t.Fatalf("describe %d, check %d, seen %v", d.calls, c.calls, c.seen)
	}
	if published != "address=10.240.0.9\npublic="+pub+"\n" {
		t.Fatalf("published %q", published)
	}
	notReady := 0
	for _, l := range g.logs {
		if strings.HasPrefix(l, "store not ready") {
			notReady++
		}
	}
	if notReady > 2 {
		t.Fatalf("%d not-ready lines for two distinct states", notReady)
	}
}

// After a pass, the marker goes as soon as a later check fails, and returns
// once the store answers again.
func TestWatch_LossAfterReadyRemovesTheMarker(t *testing.T) {
	cases := map[string]func(pub string) (*describer, *checker){
		"store stops answering": func(pub string) (*describer, *checker) {
			return &describer{answers: []describeAnswer{bound(pub)}},
				&checker{errs: []error{nil, &StoreUnreachableError{Err: errors.New("connection refused")}, nil}}
		},
		"gateway reply lost": func(pub string) (*describer, *checker) {
			return &describer{answers: []describeAnswer{bound(pub), {err: transient{}}, bound(pub)}}, &checker{}
		},
		"store no longer bound": func(pub string) (*describer, *checker) {
			return &describer{answers: []describeAnswer{bound(pub), {}, bound(pub)}}, &checker{}
		},
		"store now refuses the key": func(pub string) (*describer, *checker) {
			return &describer{answers: []describeAnswer{bound(pub)}}, &checker{errs: []error{nil, ErrStoreRefusedKey, nil}}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			g, pub := bootedGuest(t)
			d, c := mk(pub)
			var seen []bool
			if err := g.watch(g.readyCfg(d, c), func(call int, ready bool) bool {
				seen = append(seen, ready)
				return call == 4
			}); !errors.Is(err, context.Canceled) {
				t.Fatalf("watch = %v", err)
			}
			if want := []bool{false, true, false, true}; !slices.Equal(seen, want) {
				t.Fatalf("marker before each pass = %v, want %v", seen, want)
			}
		})
	}
}

// A marker left by an earlier run is cleared before the first pass.
func TestWatch_ClearsAStaleMarker(t *testing.T) {
	g, pub := bootedGuest(t)
	mustWrite(t, filepath.Join(g.cfg.RunDir, readyFile), "address=10.0.0.1\n")
	d := &describer{answers: []describeAnswer{{}}}
	_ = g.watch(g.readyCfg(d, &checker{}), func(call int, ready bool) bool {
		if call == 1 && ready {
			t.Fatal("a stale marker survived into the first pass")
		}
		return call == 2
	})
	_ = pub
}

// Failures that cannot clear by themselves wait the Refused cadence: in the
// window, each is tried once and never published.
func TestWatch_FinalFailuresDoNotSpin(t *testing.T) {
	other, _ := nkeys.CreateUser()
	otherPub, _ := other.PublicKey()
	cases := map[string]struct {
		d      func(pub string) *describer
		c      *checker
		checks int
	}{
		"store refuses the key": {func(pub string) *describer { return &describer{answers: []describeAnswer{bound(pub)}} },
			&checker{errs: []error{ErrStoreRefusedKey}}, 1},
		"store launched for another key": {func(string) *describer { return &describer{answers: []describeAnswer{bound(otherPub)}} },
			&checker{}, 0},
		"unclassified gateway refusal": {func(string) *describer {
			return &describer{answers: []describeAnswer{{err: errors.New("InvalidParameterValue")}}}
		}, &checker{}, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g, pub := bootedGuest(t)
			g.cfg.Wait = time.Millisecond
			d := tc.d(pub)
			cfg := g.readyCfg(d, tc.c)
			cfg.Refresh, cfg.Refused = time.Millisecond, time.Hour
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if err := Watch(ctx, cfg); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("watch = %v", err)
			}
			if d.calls != 1 || tc.c.calls != tc.checks {
				t.Fatalf("describe %d, check %d in the window", d.calls, tc.c.calls)
			}
			g.assertNotReady()
		})
	}
}

// A mismatched store is asked again at the Refused cadence, so a store the
// operator relaunches for this worker's key is picked up.
func TestWatch_RecoversWhenTheStoreIsRelaunchedForThisKey(t *testing.T) {
	g, pub := bootedGuest(t)
	other, _ := nkeys.CreateUser()
	otherPub, _ := other.PublicKey()
	d := &describer{answers: []describeAnswer{bound(otherPub), bound(pub)}}
	c := &checker{}
	if err := g.watch(g.readyCfg(d, c), func(_ int, ready bool) bool { return ready }); !errors.Is(err, context.Canceled) {
		t.Fatalf("watch = %v", err)
	}
	if d.calls != 2 || c.calls != 1 {
		t.Fatalf("describe %d, check %d", d.calls, c.calls)
	}
}

// A key missing from the disk is never replaced, however often it is asked.
func TestWatch_LostKeyIsNeverReplaced(t *testing.T) {
	g, pub := bootedGuest(t)
	dir := filepath.Join(g.cfg.DataDir, keyDirName)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	d := &describer{answers: []describeAnswer{bound(pub)}}
	c := &checker{}
	_ = g.watch(g.readyCfg(d, c), func(call int, ready bool) bool {
		if ready {
			t.Fatal("ready without a key")
		}
		return call == 5
	})
	if c.calls != 0 {
		t.Fatalf("checked %d times without a key", c.calls)
	}
	if after, _ := os.ReadDir(dir); len(after) != 0 {
		t.Fatal("a replacement key was written")
	}
}

// Readiness never runs on a disk that is not proven mounted.
func TestWatch_RefusesWithoutTheMountedWorkerDisk(t *testing.T) {
	g, pub := bootedGuest(t)
	g.resetMounts()
	g.disk.noopMount = true
	d := &describer{answers: []describeAnswer{bound(pub)}}
	if err := Watch(context.Background(), g.readyCfg(d, &checker{})); err == nil || d.calls != 0 {
		t.Fatalf("err = %v, describe %d", err, d.calls)
	}
}

// After a restart the same key reaches readiness again with no new key.
func TestWatch_SurvivesARestart(t *testing.T) {
	g, pub := bootedGuest(t)
	for range 2 {
		d := &describer{answers: []describeAnswer{bound(pub)}}
		if err := g.watch(g.readyCfg(d, &checker{}), func(_ int, ready bool) bool { return ready }); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		g.resetMounts()
		again, err := g.boot()
		if err != nil || again != pub {
			t.Fatalf("reboot = %q %v", again, err)
		}
	}
}

func (g *guest) assertNotReady() {
	g.t.Helper()
	if _, err := os.Stat(filepath.Join(g.cfg.RunDir, readyFile)); !os.IsNotExist(err) {
		g.t.Fatalf("readiness was published: %v", err)
	}
}

func startStore(t *testing.T, pub string, port int) *server.Server {
	t.Helper()
	return tlsStore(t, pub, port, testStore)
}

// End to end on the guest with a real store: not ready before it serves,
// ready once it does, not ready again once it stops, and the marker never
// holds the seed.
func TestWatch_RealStoreStartsLateThenStops(t *testing.T) {
	g, pub := bootedGuest(t)
	seedFile, _ := os.ReadDir(filepath.Join(g.cfg.DataDir, keyDirName))
	key, _ := os.ReadFile(filepath.Join(g.cfg.DataDir, keyDirName, seedFile[0].Name()))
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	d := &describer{answers: []describeAnswer{{st: Store{Bound: true, Address: "127.0.0.1", PublicKey: pub, ServerKeyPin: testStorePin}}}}
	cfg := g.readyCfg(d, NATSChecker{Port: port, Dial: time.Second})
	var ns *server.Server
	phase := 0
	err := g.watch(cfg, func(call int, ready bool) bool {
		switch {
		case phase == 0 && call == 3:
			if ready {
				t.Fatal("ready before the store served")
			}
			ns, phase = startStore(t, pub, port), 1
		case phase == 1 && ready:
			if strings.Contains(g.marker(), strings.TrimSpace(string(key))) {
				t.Fatal("the seed was published")
			}
			ns.Shutdown()
			ns.WaitForShutdown()
			phase = 2
		case phase == 2:
			if ready {
				t.Fatal("still ready after the store stopped")
			}
			return true
		}
		return false
	})
	if !errors.Is(err, context.Canceled) || phase != 2 {
		t.Fatalf("watch = %v, phase %d", err, phase)
	}
}

// storeServer is the pinned test store admitting only pub; it returns its port.
func storeServer(t *testing.T, pub string) int {
	t.Helper()
	return tlsStore(t, pub, 0, testStore).Addr().(*net.TCPAddr).Port
}

// plainStore is a real NATS server without TLS, as an unpinned store is.
func plainStore(t *testing.T, pub string) int {
	t.Helper()
	opts := &server.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true,
		Nkeys: []*server.NkeyUser{{Nkey: pub}},
	}
	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("store server did not start")
	}
	t.Cleanup(ns.Shutdown)
	return ns.Addr().(*net.TCPAddr).Port
}

// The real checker passes only with the admitted key; a wrong key is refused
// as final, and nothing listening is retryable.
func TestNATSChecker_AgainstARealStore(t *testing.T) {
	kp, _ := nkeys.CreateUser()
	pub, _ := kp.PublicKey()
	port := storeServer(t, pub)
	c := NATSChecker{Port: port, Dial: 2 * time.Second}
	if err := c.Check(context.Background(), "127.0.0.1", testStorePin, kp); err != nil {
		t.Fatalf("admitted key: %v", err)
	}
	other, _ := nkeys.CreateUser()
	err := c.Check(context.Background(), "127.0.0.1", testStorePin, other)
	if !errors.Is(err, ErrStoreRefusedKey) || retryable(err) {
		t.Fatalf("wrong key: %v", err)
	}
	seed, _ := kp.Seed()
	if strings.Contains(err.Error(), string(seed)) {
		t.Fatal("the error echoes a seed")
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	err = NATSChecker{Port: closed, Dial: time.Second}.Check(context.Background(), "127.0.0.1", testStorePin, kp)
	if !retryable(err) {
		t.Fatalf("nothing listening: %v is not retryable", err)
	}
	if err := c.Check(context.Background(), "store.example", testStorePin, kp); err == nil || retryable(err) {
		t.Fatalf("hostname accepted: %v", err)
	}
}
