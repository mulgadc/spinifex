package storekey

import (
	"context"
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sqsv1 "github.com/mulgadc/spinifex/contracts/sqs/v1"
)

const vol = "vol-0123456789abcdef0"

type refusal struct{ retryable, notBound bool }

func (r refusal) Error() string   { return "refused" }
func (r refusal) Retryable() bool { return r.retryable }
func (r refusal) NotBound() bool  { return r.notBound }

// fakeGateway behaves as the gateway's create-once pin record, with scripted
// refusals ahead of each call.
type fakeGateway struct {
	mu         sync.Mutex
	rec        Record
	describeEr []error
	recordEr   []error
	recorded   int
	loseReply  bool
}

func (g *fakeGateway) DescribeServerKey(context.Context) (Record, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.describeEr) > 0 {
		err := g.describeEr[0]
		g.describeEr = g.describeEr[1:]
		return Record{}, err
	}
	return g.rec, nil
}

func (g *fakeGateway) RecordServerKey(_ context.Context, claim, volume, pin string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.recordEr) > 0 {
		err := g.recordEr[0]
		g.recordEr = g.recordEr[1:]
		return err
	}
	if claim != g.rec.ClaimID || volume != g.rec.VolumeID {
		return errors.New("not this store")
	}
	if g.rec.Pin != "" && g.rec.Pin != pin {
		return errors.New("pin changed")
	}
	g.rec.Pin = pin
	g.recorded++
	if g.loseReply {
		g.loseReply = false
		return refusal{retryable: true}
	}
	return nil
}

func newGateway() *fakeGateway {
	return &fakeGateway{rec: Record{ClaimID: "claim-1", VolumeID: vol}}
}

func config(t *testing.T, dataDir string, g Gateway) Config {
	t.Helper()
	return Config{
		Dir:      filepath.Join(dataDir, "tls"),
		VolumeID: vol,
		Address:  "10.0.0.5",
		CertPath: filepath.Join(t.TempDir(), "server-cert.pem"),
		Gateway:  g,
		Attempts: 5,
		Log:      func(string) {},
	}
}

func pinOf(t *testing.T, cfg Config, keyPath string) string {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(cfg.CertPath, keyPath)
	if err != nil {
		t.Fatalf("load key pair: %v", err)
	}
	return sqsv1.Pin(pair.Leaf)
}

func TestEnsureRecordsPinOnceAndReusesKeyOnRestart(t *testing.T) {
	data, g := t.TempDir(), newGateway()
	g.describeEr = []error{refusal{notBound: true}, refusal{retryable: true}}
	cfg := config(t, data, g)
	keyPath, err := Ensure(context.Background(), cfg)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if got := pinOf(t, cfg, keyPath); got != g.rec.Pin || g.recorded != 1 {
		t.Fatalf("recorded pin %q (%d times), certificate pin %q", g.rec.Pin, g.recorded, got)
	}
	fi, err := os.Stat(keyPath)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode: %v %v", fi, err)
	}

	again := config(t, data, g)
	if _, err := Ensure(context.Background(), again); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if pinOf(t, again, keyPath) != g.rec.Pin || g.recorded != 1 {
		t.Fatalf("restart changed the key or recorded again: %d", g.recorded)
	}
}

// A reply lost after the gateway recorded the pin is repeated with the same
// pin, which the create-once record accepts.
func TestEnsureRepeatsRecordAfterLostReply(t *testing.T) {
	g := newGateway()
	g.loseReply = true
	cfg := config(t, t.TempDir(), g)
	keyPath, err := Ensure(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if pinOf(t, cfg, keyPath) != g.rec.Pin {
		t.Fatal("recorded pin does not name the disk key")
	}
}

// A key created before its pin was recorded is recorded on the next boot.
func TestEnsureRecordsExistingKeyWithNoPin(t *testing.T) {
	data, g := t.TempDir(), newGateway()
	g.recordEr = []error{errors.New("crash before record")}
	if _, err := Ensure(context.Background(), config(t, data, g)); err == nil {
		t.Fatal("first boot: want a final record failure")
	}
	cfg := config(t, data, g)
	keyPath, err := Ensure(context.Background(), cfg)
	if err != nil {
		t.Fatalf("next boot: %v", err)
	}
	if pinOf(t, cfg, keyPath) != g.rec.Pin {
		t.Fatal("next boot recorded a different key")
	}
}

func TestEnsureRefusesWithoutReplacingKey(t *testing.T) {
	t.Run("recorded pin, missing key", func(t *testing.T) {
		data, g := t.TempDir(), newGateway()
		g.rec.Pin = strings.Repeat("ab", 32)
		_, err := Ensure(context.Background(), config(t, data, g))
		if !errors.Is(err, ErrKeyMissing) {
			t.Fatalf("got %v, want ErrKeyMissing", err)
		}
		if _, err := os.Stat(filepath.Join(data, "tls", keyFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a key was created: %v", err)
		}
	})
	t.Run("recorded pin, different key", func(t *testing.T) {
		data, g := t.TempDir(), newGateway()
		if _, err := Ensure(context.Background(), config(t, data, g)); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(filepath.Join(data, "tls", keyFile))
		g.rec.Pin = "ff" + g.rec.Pin[2:]
		_, err := Ensure(context.Background(), config(t, data, g))
		if !errors.Is(err, ErrKeyChanged) {
			t.Fatalf("got %v, want ErrKeyChanged", err)
		}
		after, _ := os.ReadFile(filepath.Join(data, "tls", keyFile))
		if string(before) != string(after) {
			t.Fatal("the disk key was replaced")
		}
	})
	t.Run("another store's volume", func(t *testing.T) {
		data, g := t.TempDir(), newGateway()
		g.rec.VolumeID = "vol-0fffffffffffffff0"
		_, err := Ensure(context.Background(), config(t, data, g))
		if !errors.Is(err, ErrWrongStore) {
			t.Fatalf("got %v, want ErrWrongStore", err)
		}
		if _, err := os.Stat(filepath.Join(data, "tls", keyFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a key was created: %v", err)
		}
	})
	t.Run("final refusal is not retried", func(t *testing.T) {
		g := newGateway()
		g.describeEr = []error{refusal{}, refusal{retryable: true}}
		if _, err := Ensure(context.Background(), config(t, t.TempDir(), g)); err == nil {
			t.Fatal("want the first, final refusal")
		}
		if len(g.describeEr) != 1 {
			t.Fatal("a final refusal was retried")
		}
	})
}
