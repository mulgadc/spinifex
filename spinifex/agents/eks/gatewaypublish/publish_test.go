package gatewaypublish

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

// gateway starts a TLS gateway answering every POST with status, and returns a
// config pointing at it with the CA written to disk.
func gateway(t *testing.T, status int, got *publishBody, hits *atomic.Int32) Config {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/clusters/demo/internal-publish" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got != nil {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, got)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return Config{
		GatewayURL: srv.URL, GatewayCA: ca, AccessKey: "AKIATEST", SecretKey: "secret",
		Region: "us-east-1", AccountID: "123456789012", ClusterName: "demo", Channel: "state",
		Payload: strings.NewReader(`{"ok":true}`),
	}
}

func TestRun_RejectsBeforeContactingGateway(t *testing.T) {
	valid := Config{AccountID: "123456789012", ClusterName: "demo", Channel: "state", Payload: strings.NewReader(`{}`)}
	cases := []struct {
		name string
		mod  func(*Config)
		want string
	}{
		{"account", func(c *Config) { c.AccountID = "" }, "--account-id is required (or set EKS_ACCOUNT_ID)"},
		{"cluster", func(c *Config) { c.ClusterName = "" }, "--cluster is required (or set EKS_CLUSTER_NAME)"},
		{"channel", func(c *Config) { c.Channel = "events" }, "--channel must be bootstrap, state or addon"},
		{"bootstrap kind", func(c *Config) { c.Channel = "bootstrap" }, "--kind is required for the bootstrap channel"},
		{"read error", func(c *Config) { c.Payload = errReader{} }, "read stdin payload: boom"},
		{"empty", func(c *Config) { c.Payload = strings.NewReader(" \n") }, "empty stdin payload"},
		{"invalid json", func(c *Config) { c.Payload = strings.NewReader("{") }, "stdin payload is not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.mod(&cfg)
			err := Run(context.Background(), cfg)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Run error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRun_PublishesWrappedPayload(t *testing.T) {
	var got publishBody
	var hits atomic.Int32
	cfg := gateway(t, http.StatusOK, &got, &hits)
	cfg.Channel, cfg.Kind = "bootstrap", "k3s-bootstrap-token"

	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("gateway hit %d times, want 1", hits.Load())
	}
	if got.AccountID != "123456789012" || got.Channel != "bootstrap" || got.Kind != "k3s-bootstrap-token" || string(got.Payload) != `{"ok":true}` {
		t.Fatalf("published body = %+v", got)
	}
}

func TestRun_FailsAfterAttemptBudget(t *testing.T) {
	prev := retryDelay
	retryDelay = 0
	t.Cleanup(func() { retryDelay = prev })
	var hits atomic.Int32
	cfg := gateway(t, http.StatusServiceUnavailable, nil, &hits)

	err := Run(context.Background(), cfg)
	if err == nil || !strings.HasPrefix(err.Error(), "publish failed after 30 attempts: ") {
		t.Fatalf("Run error = %v, want attempt-budget failure", err)
	}
	if hits.Load() != maxAttempts {
		t.Fatalf("gateway hit %d times, want %d", hits.Load(), maxAttempts)
	}
}

func TestRun_StopsRetryingWhenCancelled(t *testing.T) {
	prev := retryDelay
	retryDelay = time.Hour
	t.Cleanup(func() { retryDelay = prev })
	var hits atomic.Int32
	cfg := gateway(t, http.StatusServiceUnavailable, nil, &hits)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for hits.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	if err := Run(ctx, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("gateway hit %d times, want 1", hits.Load())
	}
}
