package gatewayfetch

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// gateway starts a TLS gateway that answers wantPath with status and body, and
// returns a config pointing at it with the CA written to disk.
func gateway(t *testing.T, wantPath string, status int, body string, hits *atomic.Int32) (Config, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != wantPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	return Config{
		GatewayURL: srv.URL, GatewayCA: ca, AccessKey: "AKIATEST", SecretKey: "secret",
		Region: "us-east-1", AccountID: "123456789012", ClusterName: "demo", Resource: "addons", Out: &out,
	}, &out
}

func TestRun_RejectsBeforeContactingGateway(t *testing.T) {
	valid := Config{GatewayURL: "https://gw", AccountID: "123456789012", ClusterName: "demo", Resource: "addons", Out: &bytes.Buffer{}}
	cases := []struct {
		name string
		mod  func(*Config)
		want string
	}{
		{"account", func(c *Config) { c.AccountID = "" }, "--account-id is required (or set EKS_ACCOUNT_ID)"},
		{"cluster", func(c *Config) { c.ClusterName = "" }, "--cluster is required (or set EKS_CLUSTER_NAME)"},
		{"resource", func(c *Config) { c.Resource = "secrets" }, "--resource must be addons or recovery"},
		{"recovery instance", func(c *Config) { c.Resource = "recovery" }, "--instance-id is required for --resource recovery (or set EKS_INSTANCE_ID)"},
		{"gateway client", func(c *Config) { c.GatewayURL = "" }, "build gateway client: eksgw: baseURL is required"},
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

func TestRun_RendersAddons(t *testing.T) {
	var hits atomic.Int32
	cfg, out := gateway(t, "/clusters/demo/internal-addons/123456789012", http.StatusOK,
		`{"addons":[{"addonName":"spinifex-noop","addonVersion":"0.1.0"}]}`, &hits)

	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := out.String(), "spinifex-noop\t0.1.0\t\t\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRun_RendersRecoveryDirective(t *testing.T) {
	var hits atomic.Int32
	cfg, out := gateway(t, "/clusters/demo/internal-recovery/123456789012/i-0abc", http.StatusOK,
		`{"directive":{"epoch":7,"action":"cluster-reset","snapshot":"snap-1","snapshotRequired":true}}`, &hits)
	cfg.Resource, cfg.InstanceID = "recovery", "i-0abc"

	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := out.String(), "7\tcluster-reset\tsnap-1\t1\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRun_RenderFailureIsNotRetried(t *testing.T) {
	var hits atomic.Int32
	cfg, _ := gateway(t, "/clusters/demo/internal-addons/123456789012", http.StatusOK, `{not-json`, &hits)

	err := Run(context.Background(), cfg)
	if err == nil || !strings.HasPrefix(err.Error(), "render response: unmarshal internal-addons response: ") {
		t.Fatalf("Run error = %v, want render failure", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("gateway hit %d times, want 1", hits.Load())
	}
}

func TestRun_FailsAfterAttemptBudget(t *testing.T) {
	prev := retryDelay
	retryDelay = 0
	t.Cleanup(func() { retryDelay = prev })
	var hits atomic.Int32
	cfg, out := gateway(t, "/clusters/demo/internal-addons/123456789012", http.StatusServiceUnavailable, "", &hits)

	err := Run(context.Background(), cfg)
	if err == nil || !strings.HasPrefix(err.Error(), "fetch failed after 30 attempts: ") {
		t.Fatalf("Run error = %v, want attempt-budget failure", err)
	}
	if hits.Load() != maxAttempts || out.Len() != 0 {
		t.Fatalf("gateway hits = %d, stdout = %q; want %d hits and no output", hits.Load(), out.String(), maxAttempts)
	}
}

func TestRun_StopsRetryingWhenCancelled(t *testing.T) {
	prev := retryDelay
	retryDelay = time.Hour
	t.Cleanup(func() { retryDelay = prev })
	var hits atomic.Int32
	cfg, _ := gateway(t, "/clusters/demo/internal-addons/123456789012", http.StatusServiceUnavailable, "", &hits)
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
