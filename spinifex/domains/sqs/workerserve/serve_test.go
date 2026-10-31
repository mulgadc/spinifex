package workerserve

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/relay"
)

type postGateway struct {
	outcome string
	err     error
}

func (postGateway) PollWork(context.Context) ([]relay.Work, error) { return nil, nil }

func (g postGateway) CompleteWork(context.Context, string, []byte) (string, error) {
	return g.outcome, g.err
}

type claimingExec struct{ released []relay.Claim }

func (e *claimingExec) Execute(context.Context, relay.Work) ([]byte, []relay.Claim) {
	return []byte(`{"output":[]}`), []relay.Claim{{Account: "123456789012", Region: "ap-southeast-2", Queue: "orders", ReceiptHandle: "h"}}
}

func (e *claimingExec) Release(_ context.Context, c []relay.Claim) error {
	e.released = append(e.released, c...)
	return nil
}

// Claimed messages are released only when the gateway proves the caller
// left before the result arrived. A lost reply, an error or any other answer
// may have reached the caller, so those messages wait out their visibility.
func TestHandle_ReleasesOnlyWhenTheCallerProvablyLeft(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gw      postGateway
		release bool
	}{
		{"caller gone", postGateway{outcome: OutcomeCallerGone}, true},
		{"delivered", postGateway{outcome: OutcomeDelivered}, false},
		{"unknown", postGateway{outcome: "unknown"}, false},
		{"lost reply", postGateway{err: errors.New("connection reset")}, false},
		{"refused", postGateway{outcome: OutcomeCallerGone, err: errors.New("AccessDenied")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &claimingExec{}
			cfg := Config{Gateway: tc.gw, Executor: exec}
			handle(context.Background(), cfg, relay.Work{ID: "w", Op: relay.OpReceiveMessage, Deadline: time.Now().Add(time.Second)}, func(string) {})
			if got := len(exec.released) > 0; got != tc.release {
				t.Fatalf("released = %v, want %v", got, tc.release)
			}
		})
	}
}

// The store records a fingerprint the key cannot be read from; a restart
// with the same key serves and any other key is refused.
func TestCheckReceiptKey_BindsTheFirstKeyByFingerprintOnly(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	key := bytes.Repeat([]byte{0x5a}, 32)
	if err := CheckReceiptKey(ctx, js, key); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	kv, err := js.KeyValue(ctx, markerBucket)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := kv.Get(ctx, markerKey)
	if err != nil {
		t.Fatal(err)
	}
	if v := entry.Value(); bytes.Contains(v, key) || bytes.Contains(v, []byte(hex.EncodeToString(key))) {
		t.Fatal("marker holds the receipt key")
	}
	if err := CheckReceiptKey(ctx, js, key); err != nil {
		t.Fatalf("restart with the same key: %v", err)
	}
	if err := CheckReceiptKey(ctx, js, bytes.Repeat([]byte{0xa5}, 32)); !errors.Is(err, ErrReceiptKeyChanged) {
		t.Fatalf("changed key = %v; want ErrReceiptKeyChanged", err)
	}
}

type flakyGateway struct {
	mu       sync.Mutex
	failures int
	offered  bool
	posted   []string
}

func (g *flakyGateway) PollWork(ctx context.Context) ([]relay.Work, error) {
	g.mu.Lock()
	switch {
	case g.failures > 0:
		g.failures--
		g.mu.Unlock()
		return nil, errors.New("gateway unreachable")
	case !g.offered:
		g.offered = true
		g.mu.Unlock()
		return []relay.Work{{ID: "w1", Op: relay.OpListQueues, Deadline: time.Now().Add(time.Minute)}}, nil
	}
	g.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (g *flakyGateway) CompleteWork(_ context.Context, id string, _ []byte) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.posted = append(g.posted, id)
	return OutcomeDelivered, nil
}

// Repeated poll failures are logged once and do not stop the loop: work the
// gateway offers after it recovers is still run and posted.
func TestServe_KeepsPollingThroughGatewayFailures(t *testing.T) {
	gw := &flakyGateway{failures: 3}
	var logs []string
	var mu sync.Mutex
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Config{Gateway: gw, Executor: &claimingExec{}, Backoff: time.Millisecond, Log: func(s string) {
			mu.Lock()
			logs = append(logs, s)
			mu.Unlock()
		}})
	}()
	for {
		gw.mu.Lock()
		n := len(gw.posted)
		gw.mu.Unlock()
		if n > 0 {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("work offered after recovery was never posted")
		}
		runtime.Gosched()
	}
	cancel()
	<-done
	if len(gw.posted) != 1 || gw.posted[0] != "w1" {
		t.Fatalf("posted %v; want w1 once", gw.posted)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 2 || !strings.HasPrefix(logs[0], "poll failed") {
		t.Fatalf("logs = %q; want one failure line and one recovery line", logs)
	}
}
