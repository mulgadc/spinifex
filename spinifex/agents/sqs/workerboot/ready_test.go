package workerboot

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
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

func (c *checker) Check(_ context.Context, address string, kp nkeys.KeyPair) error {
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

func (g *guest) ready(d *describer, c Checker) (Store, error) {
	g.t.Helper()
	return Ready(context.Background(), ReadyConfig{
		Config:       g.cfg,
		NewDescriber: func(string, string) (StoreDescriber, error) { return d, nil },
		Checker:      c,
	})
}

func bound(pub string) describeAnswer {
	return describeAnswer{st: Store{Bound: true, Address: "10.240.0.9", PublicKey: pub}}
}

// A store launched after the worker booted: pending, then bound but not yet
// answering, then ready. Only then is readiness published.
func TestReady_WaitsForADelayedStore(t *testing.T) {
	g, pub := bootedGuest(t)
	g.cfg.Attempts = 5
	d := &describer{answers: []describeAnswer{{}, bound(pub)}}
	c := &checker{errs: []error{&StoreUnreachableError{Err: errors.New("connection refused")}}}
	st, err := g.ready(d, c)
	if err != nil || st.Address != "10.240.0.9" {
		t.Fatalf("ready = %+v %v", st, err)
	}
	if d.calls != 3 || c.calls != 2 {
		t.Fatalf("describe %d, check %d; want 3 and 2", d.calls, c.calls)
	}
	if c.seen[0] != "10.240.0.9|"+pub {
		t.Fatalf("checked %q", c.seen[0])
	}
	got, err := os.ReadFile(filepath.Join(g.cfg.RunDir, readyFile))
	if err != nil || string(got) != "address=10.240.0.9\npublic="+pub+"\n" {
		t.Fatalf("published %q %v", got, err)
	}
}

// A store that is never launched exhausts the bound and publishes nothing.
func TestReady_NeverBoundGivesUpWithinTheBound(t *testing.T) {
	g, _ := bootedGuest(t)
	d := &describer{answers: []describeAnswer{{}}}
	c := &checker{}
	if _, err := g.ready(d, c); !errors.Is(err, errStorePending) {
		t.Fatalf("err = %v, want pending", err)
	}
	if d.calls != g.cfg.Attempts || c.calls != 0 {
		t.Fatalf("describe %d, check %d", d.calls, c.calls)
	}
	g.assertNotReady()
}

// A store bound to another key never loads or checks this worker's key, and a
// key missing from the disk is never replaced.
func TestReady_WrongOrMissingKeyFailsAtOnce(t *testing.T) {
	t.Run("store launched for another key", func(t *testing.T) {
		g, _ := bootedGuest(t)
		other, _ := nkeys.CreateUser()
		otherPub, _ := other.PublicKey()
		c := &checker{}
		_, err := g.ready(&describer{answers: []describeAnswer{bound(otherPub)}}, c)
		if !errors.Is(err, ErrStoreKeyMismatch) || c.calls != 0 {
			t.Fatalf("err = %v, checks %d", err, c.calls)
		}
		g.assertNotReady()
	})
	t.Run("key lost from the disk", func(t *testing.T) {
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
		if _, err := g.ready(d, c); err == nil || c.calls != 0 || d.calls != 1 {
			t.Fatalf("err = %v, describe %d, checks %d", err, d.calls, c.calls)
		}
		if after, _ := os.ReadDir(dir); len(after) != 0 {
			t.Fatal("a replacement key was written")
		}
	})
	t.Run("store refuses the key", func(t *testing.T) {
		g, pub := bootedGuest(t)
		c := &checker{errs: []error{ErrStoreRefusedKey}}
		if _, err := g.ready(&describer{answers: []describeAnswer{bound(pub)}}, c); !errors.Is(err, ErrStoreRefusedKey) || c.calls != 1 {
			t.Fatalf("err = %v, checks %d", err, c.calls)
		}
		g.assertNotReady()
	})
}

// Readiness never runs on a disk that is not proven mounted.
func TestReady_RefusesWithoutTheMountedWorkerDisk(t *testing.T) {
	g, pub := bootedGuest(t)
	g.resetMounts()
	g.disk.noopMount = true
	d := &describer{answers: []describeAnswer{bound(pub)}}
	if _, err := g.ready(d, &checker{}); err == nil || d.calls != 0 {
		t.Fatalf("err = %v, describe %d", err, d.calls)
	}
}

// Gateway denials while pending and lost replies are asked again; any other
// refusal is final.
func TestReady_RetriesOnlyClassifiedErrors(t *testing.T) {
	g, pub := bootedGuest(t)
	d := &describer{answers: []describeAnswer{{err: notBoundError{}}, {err: transient{}}, bound(pub)}}
	if _, err := g.ready(d, &checker{}); err != nil || d.calls != 3 {
		t.Fatalf("err = %v, describe %d", err, d.calls)
	}
	g2, _ := bootedGuest(t)
	d2 := &describer{answers: []describeAnswer{{err: errors.New("InvalidParameterValue")}}}
	if _, err := g2.ready(d2, &checker{}); err == nil || d2.calls != 1 {
		t.Fatalf("err = %v, describe %d", err, d2.calls)
	}
}

// After a restart the same key reaches readiness again with no new key.
func TestReady_SurvivesARestart(t *testing.T) {
	g, pub := bootedGuest(t)
	for range 2 {
		if _, err := g.ready(&describer{answers: []describeAnswer{bound(pub)}}, &checker{}); err != nil {
			t.Fatal(err)
		}
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

// storeServer is a real NATS server with JetStream that admits only pub.
func storeServer(t *testing.T, pub string) int {
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
	if err := c.Check(context.Background(), "127.0.0.1", kp); err != nil {
		t.Fatalf("admitted key: %v", err)
	}
	other, _ := nkeys.CreateUser()
	err := c.Check(context.Background(), "127.0.0.1", other)
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
	err = NATSChecker{Port: closed, Dial: time.Second}.Check(context.Background(), "127.0.0.1", kp)
	if !retryable(err) {
		t.Fatalf("nothing listening: %v is not retryable", err)
	}
	if err := c.Check(context.Background(), "store.example", kp); err == nil || retryable(err) {
		t.Fatalf("hostname accepted: %v", err)
	}
}

// End to end on the guest: a real store that starts after the worker is
// ready only once it serves, and the published file holds no seed.
func TestReady_RealStoreStartingLate(t *testing.T) {
	g, pub := bootedGuest(t)
	g.cfg.Attempts = 3
	g.cfg.Wait = 10 * time.Millisecond
	seedFile, _ := os.ReadDir(filepath.Join(g.cfg.DataDir, keyDirName))
	if len(seedFile) == 0 {
		t.Fatal("no key on the disk")
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	c := NATSChecker{Port: port, Dial: time.Second}
	d := &describer{answers: []describeAnswer{{st: Store{Bound: true, Address: "127.0.0.1", PublicKey: pub}}}}
	if _, err := g.ready(d, c); !retryable(err) {
		t.Fatalf("no store yet: %v", err)
	}
	opts := &server.Options{Host: "127.0.0.1", Port: port, JetStream: true, StoreDir: t.TempDir(), NoLog: true,
		NoSigs: true, Nkeys: []*server.NkeyUser{{Nkey: pub}}}
	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("store server did not start")
	}
	t.Cleanup(ns.Shutdown)
	if _, err := g.ready(d, c); err != nil {
		t.Fatalf("store serving: %v", err)
	}
	published, _ := os.ReadFile(filepath.Join(g.cfg.RunDir, readyFile))
	key, _ := os.ReadFile(filepath.Join(g.cfg.DataDir, keyDirName, seedFile[0].Name()))
	if strings.Contains(string(published), strings.TrimSpace(string(key))) {
		t.Fatal("the seed was published")
	}
}
