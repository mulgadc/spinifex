package lbagent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/predastore/auth"
)

func TestNew(t *testing.T) {
	agent, err := New("lb-test123", "https://gw:9999", "AKID", "SECRET", "us-east-1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if agent.lbID != "lb-test123" {
		t.Errorf("lbID = %q, want %q", agent.lbID, "lb-test123")
	}
	if agent.region != "us-east-1" {
		t.Errorf("region = %q, want %q", agent.region, "us-east-1")
	}
}

func TestNew_EmptyLBID(t *testing.T) {
	_, err := New("", "https://gw:9999", "AKID", "SECRET", "us-east-1")
	if err == nil {
		t.Fatal("expected error for empty lbID")
	}
}

func TestNew_EmptyGatewayURL(t *testing.T) {
	_, err := New("lb-test", "", "AKID", "SECRET", "us-east-1")
	if err == nil {
		t.Fatal("expected error for empty gatewayURL")
	}
}

func TestNew_EmptyCredentials(t *testing.T) {
	_, err := New("lb-test", "https://gw:9999", "", "SECRET", "us-east-1")
	if err == nil {
		t.Fatal("expected error for empty access key")
	}
	_, err = New("lb-test", "https://gw:9999", "AKID", "", "us-east-1")
	if err == nil {
		t.Fatal("expected error for empty secret key")
	}
}

func TestNew_EmptyRegion(t *testing.T) {
	_, err := New("lb-test", "https://gw:9999", "AKID", "SECRET", "")
	if err == nil {
		t.Fatal("expected error for empty region")
	}
}

func TestNew_SocketPath(t *testing.T) {
	agent, err := New("lb-sock123", "https://gw:9999", "AKID", "SECRET", "us-east-1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	expected := "/tmp/spinifex-haproxy/lb-sock123.sock"
	if agent.socketPath != expected {
		t.Errorf("socketPath = %q, want %q", agent.socketPath, expected)
	}
}

// TestSignedPost_ProducesVerifiableSignature confirms the migrated signing path
// (predastore/auth.SignReq over aws-sdk-go-v2) produces a request the gateway's
// SigV4 verifier accepts, including a body-hash that matches X-Amz-Content-Sha256.
func TestSignedPost_ProducesVerifiableSignature(t *testing.T) {
	const (
		accessKey = "AKIAIOSFODNN7EXAMPLE"
		secretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		region    = "us-east-1"
	)

	var parsed bool
	var verifyErr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		req, err := auth.ParseReq(r)
		if err != nil {
			verifyErr = err
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		parsed = true
		verifyErr = req.Verify(secretKey, "elasticloadbalancing", region,
			auth.WithBodyHash(hex.EncodeToString(sum[:])))
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	agent, err := New("lb-sig", srv.URL, accessKey, secretKey, region)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := agent.signedPost(url.Values{
		"Action":         {"LBAgentHeartbeat"},
		"LoadBalancerId": {"lb-sig"},
	}); err != nil {
		t.Fatalf("signedPost: %v", err)
	}
	if !parsed {
		t.Fatal("gateway did not parse a SigV4 Authorization header")
	}
	if verifyErr != nil {
		t.Fatalf("signed request failed server-side verification: %v", verifyErr)
	}
}

// fakeGateway returns a test server that responds to LBAgentHeartbeat and GetLBConfig.
func fakeGateway(t *testing.T, configHash, configText, status string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		action := r.FormValue("Action")
		w.Header().Set("Content-Type", "text/xml")

		switch action {
		case "LBAgentHeartbeat":
			st := status
			if st == "" {
				st = "active"
			}
			fmt.Fprintf(w, `<LBAgentHeartbeatResponse><LBAgentHeartbeatResult><Status>%s</Status><ConfigHash>%s</ConfigHash></LBAgentHeartbeatResult></LBAgentHeartbeatResponse>`, st, configHash)
		case "GetLBConfig":
			fmt.Fprintf(w, `<GetLBConfigResponse><GetLBConfigResult><ConfigText>%s</ConfigText><ConfigHash>%s</ConfigHash></GetLBConfigResult></GetLBConfigResponse>`, configText, configHash)
		default:
			http.Error(w, "unknown action: "+action, http.StatusBadRequest)
		}
	}))
}

func newTestAgent(t *testing.T, gwURL string) *Agent {
	t.Helper()
	agent, err := New("lb-test", gwURL, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dir := t.TempDir()
	agent.configPath = filepath.Join(dir, "haproxy.cfg")
	agent.pidPath = filepath.Join(dir, "haproxy.pid")
	agent.reloadFn = func(_, _ string) error { return nil }
	agent.statsFn = func(_ string) ([]ServerStatus, error) { return nil, nil }
	return agent
}

func TestHeartbeat_NoConfigChange(t *testing.T) {
	gw := fakeGateway(t, "hash1", "", "active")
	defer gw.Close()

	agent := newTestAgent(t, gw.URL)
	agent.localConfigHash = "hash1" // Already up to date

	agent.tick()

	// Config file should NOT exist since hash didn't change
	if _, err := os.Stat(agent.configPath); !os.IsNotExist(err) {
		t.Error("config file should not exist when hash matches")
	}
}

func TestHeartbeat_ConfigChange(t *testing.T) {
	configText := "global\n  log stdout\n"
	gw := fakeGateway(t, "hash-new", configText, "active")
	defer gw.Close()

	agent := newTestAgent(t, gw.URL)
	agent.localConfigHash = "hash-old"

	reloadCalled := false
	agent.reloadFn = func(_, _ string) error {
		reloadCalled = true
		return nil
	}

	agent.tick()

	// Config should have been written
	data, err := os.ReadFile(agent.configPath)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if string(data) != configText {
		t.Errorf("config = %q, want %q", string(data), configText)
	}
	if !reloadCalled {
		t.Error("reload was not called")
	}
	if agent.localConfigHash != "hash-new" {
		t.Errorf("localConfigHash = %q, want %q", agent.localConfigHash, "hash-new")
	}
}

func TestHeartbeat_FirstBoot(t *testing.T) {
	configText := "frontend fe1\n  bind :80\n"
	gw := fakeGateway(t, "hash-first", configText, "provisioning")
	defer gw.Close()

	agent := newTestAgent(t, gw.URL)
	// localConfigHash is "" (zero value) — different from "hash-first"

	agent.tick()

	data, err := os.ReadFile(agent.configPath)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if string(data) != configText {
		t.Errorf("config = %q, want %q", string(data), configText)
	}
}

func TestHeartbeat_IncludesHealthReport(t *testing.T) {
	var receivedBackend, receivedServer, receivedStatus string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		action := r.FormValue("Action")
		if action == "LBAgentHeartbeat" {
			receivedBackend = r.FormValue("Servers.member.1.Backend")
			receivedServer = r.FormValue("Servers.member.1.Server")
			receivedStatus = r.FormValue("Servers.member.1.Status")
			fmt.Fprintf(w, `<LBAgentHeartbeatResponse><LBAgentHeartbeatResult><Status>active</Status><ConfigHash>h1</ConfigHash></LBAgentHeartbeatResult></LBAgentHeartbeatResponse>`)
		} else {
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer gw.Close()

	agent := newTestAgent(t, gw.URL)
	agent.localConfigHash = "h1" // match to avoid config fetch
	agent.statsFn = func(_ string) ([]ServerStatus, error) {
		return []ServerStatus{
			{Backend: "bk_tg1", Server: "srv_i-web1", Status: "UP"},
		}, nil
	}

	agent.tick()

	if receivedBackend != "bk_tg1" {
		t.Errorf("backend = %q, want %q", receivedBackend, "bk_tg1")
	}
	if receivedServer != "srv_i-web1" {
		t.Errorf("server = %q, want %q", receivedServer, "srv_i-web1")
	}
	if receivedStatus != "UP" {
		t.Errorf("status = %q, want %q", receivedStatus, "UP")
	}
}

func TestHeartbeat_GatewayError(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer gw.Close()

	agent := newTestAgent(t, gw.URL)
	agent.tick() // Should not panic, just log error
}

func TestHeartbeat_ReloadError(t *testing.T) {
	configText := "global\n"
	gw := fakeGateway(t, "hash-new", configText, "active")
	defer gw.Close()

	agent := newTestAgent(t, gw.URL)
	agent.reloadFn = func(_, _ string) error {
		return fmt.Errorf("haproxy binary not found")
	}

	agent.tick()

	// localConfigHash should NOT be updated on reload failure
	if agent.localConfigHash != "" {
		t.Errorf("localConfigHash should be empty after reload failure, got %q", agent.localConfigHash)
	}
}

func TestStartStop(t *testing.T) {
	gw := fakeGateway(t, "h1", "", "active")
	defer gw.Close()

	agent := newTestAgent(t, gw.URL)
	agent.localConfigHash = "h1"

	var started atomic.Bool
	errCh := make(chan error, 1)
	go func() {
		started.Store(true)
		errCh <- agent.Start()
	}()

	// Wait for the agent to start.
	time.Sleep(100 * time.Millisecond)
	if !started.Load() {
		t.Fatal("agent did not start")
	}

	agent.Stop()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not stop in time")
	}
}

func TestStopIdempotent(t *testing.T) {
	gw := fakeGateway(t, "h1", "", "active")
	defer gw.Close()

	agent := newTestAgent(t, gw.URL)
	agent.localConfigHash = "h1"

	go agent.Start()
	time.Sleep(50 * time.Millisecond)

	// Calling Stop multiple times should not panic.
	agent.Stop()
	agent.Stop()
}

func TestHeartbeat_StatsError(t *testing.T) {
	gw := fakeGateway(t, "h1", "", "active")
	defer gw.Close()

	agent := newTestAgent(t, gw.URL)
	agent.localConfigHash = "h1"
	agent.statsFn = func(_ string) ([]ServerStatus, error) {
		return nil, fmt.Errorf("socket not found")
	}

	// Should still send heartbeat with empty servers, not panic
	agent.tick()
}

func TestEnginePaths_Nginx(t *testing.T) {
	agent := newTestAgent(t, "http://example.invalid")
	var nginxCalled, haproxyCalled bool
	agent.reloadNginxFn = func(_, _ string) error { nginxCalled = true; return nil }
	agent.reloadFn = func(_, _ string) error { haproxyCalled = true; return nil }

	cfg, pid, certDir, reload := agent.enginePaths(EngineNginx)
	if cfg != NginxConfigPath || pid != NginxPIDPath || certDir != NginxCertDir {
		t.Errorf("nginx paths = %q,%q,%q want %q,%q,%q", cfg, pid, certDir, NginxConfigPath, NginxPIDPath, NginxCertDir)
	}
	_ = reload("", "")
	if !nginxCalled || haproxyCalled {
		t.Errorf("nginx engine routed to wrong reload (nginx=%v haproxy=%v)", nginxCalled, haproxyCalled)
	}
}

func TestEnginePaths_HAProxyDefault(t *testing.T) {
	agent := newTestAgent(t, "http://example.invalid")
	var nginxCalled, haproxyCalled bool
	agent.reloadNginxFn = func(_, _ string) error { nginxCalled = true; return nil }
	agent.reloadFn = func(_, _ string) error { haproxyCalled = true; return nil }

	// Both explicit haproxy and an empty engine fall through to HAProxy.
	for _, engine := range []string{EngineHAProxy, ""} {
		cfg, pid, certDir, reload := agent.enginePaths(engine)
		if cfg != agent.configPath || pid != agent.pidPath || certDir != agent.certDir {
			t.Errorf("engine %q paths = %q,%q,%q want haproxy defaults", engine, cfg, pid, certDir)
		}
		_ = reload("", "")
	}
	if nginxCalled || !haproxyCalled {
		t.Errorf("haproxy/empty engine routed to wrong reload (nginx=%v haproxy=%v)", nginxCalled, haproxyCalled)
	}
}

func TestTick_NginxSkipsStats(t *testing.T) {
	gw := fakeGateway(t, "h1", "", "active")
	defer gw.Close()

	agent := newTestAgent(t, gw.URL)
	agent.localConfigHash = "h1"
	agent.engine = EngineNginx // stats poll is HAProxy-only
	statsCalled := false
	agent.statsFn = func(_ string) ([]ServerStatus, error) {
		statsCalled = true
		return nil, nil
	}

	agent.tick()

	if statsCalled {
		t.Error("nginx engine must not poll HAProxy stats")
	}
}

func TestTick_NginxProbesHealthTargets(t *testing.T) {
	var receivedServer, receivedStatus string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.FormValue("Action") == "LBAgentHeartbeat" {
			receivedServer = r.FormValue("Servers.member.1.Server")
			receivedStatus = r.FormValue("Servers.member.1.Status")
			fmt.Fprint(w, `<LBAgentHeartbeatResponse><LBAgentHeartbeatResult><Status>active</Status><ConfigHash>h1</ConfigHash></LBAgentHeartbeatResult></LBAgentHeartbeatResponse>`)
			return
		}
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	defer gw.Close()

	agent := newTestAgent(t, gw.URL)
	agent.localConfigHash = "h1" // match to avoid config fetch
	agent.engine = EngineNginx
	agent.healthTargets = []HealthTarget{{ServerName: "srv_i-web1", Address: "10.0.0.5:80", Protocol: "TCP"}}
	statsCalled := false
	agent.statsFn = func(_ string) ([]ServerStatus, error) { statsCalled = true; return nil, nil }
	probedTargets := 0
	agent.probeFn = func(targets []HealthTarget) []ServerStatus {
		probedTargets = len(targets)
		return []ServerStatus{{Server: "srv_i-web1", Status: "UP"}}
	}

	agent.tick()

	if statsCalled {
		t.Error("nginx engine must not poll HAProxy stats")
	}
	if probedTargets != 1 {
		t.Errorf("probeFn saw %d targets, want 1", probedTargets)
	}
	if receivedServer != "srv_i-web1" || receivedStatus != "UP" {
		t.Errorf("heartbeat server/status = %q/%q, want srv_i-web1/UP", receivedServer, receivedStatus)
	}
}

func TestHeartbeat_EmptyConfigHash(t *testing.T) {
	// Gateway returns empty config hash (no config stored yet)
	gw := fakeGateway(t, "", "", "provisioning")
	defer gw.Close()

	agent := newTestAgent(t, gw.URL)

	agent.tick()

	// Should not attempt config fetch when hash is empty
	if _, err := os.Stat(agent.configPath); !os.IsNotExist(err) {
		t.Error("config file should not exist when gateway returns empty hash")
	}
}

func TestWriteConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "haproxy.cfg")

	content := "global\n  log stdout\n"
	if err := WriteConfig(path, content); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != content {
		t.Errorf("config = %q, want %q", string(data), content)
	}
}

func TestWriteConfig_CreatesDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "haproxy.cfg")

	if err := WriteConfig(path, "test"); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file not created: %v", err)
	}
}

func TestWriteConfig_Atomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "haproxy.cfg")

	if err := WriteConfig(path, "initial"); err != nil {
		t.Fatalf("WriteConfig initial: %v", err)
	}

	if err := WriteConfig(path, "updated"); err != nil {
		t.Fatalf("WriteConfig updated: %v", err)
	}

	data, _ := os.ReadFile(path)
	if string(data) != "updated" {
		t.Errorf("config = %q, want %q", string(data), "updated")
	}

	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file should not exist after successful write")
	}
}

func TestIsHAProxyRunning_NoPIDFile(t *testing.T) {
	if readPID("/nonexistent/haproxy.pid") != 0 {
		t.Error("expected 0 for non-existent PID file")
	}
}

func TestReadPID_InvalidContent(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "haproxy.pid")

	os.WriteFile(pidFile, []byte("not-a-number\n"), 0o644)
	if pid := readPID(pidFile); pid != 0 {
		t.Errorf("readPID = %d, want 0 for invalid content", pid)
	}
}

func TestReadPID_DeadProcess(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "haproxy.pid")

	os.WriteFile(pidFile, []byte("999999999\n"), 0o644)
	if pid := readPID(pidFile); pid != 0 {
		t.Errorf("readPID = %d, want 0 for dead process", pid)
	}
}

func TestHeartbeat_InvalidXMLResponse(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		w.Write([]byte("not valid xml"))
	}))
	defer gw.Close()

	agent := newTestAgent(t, gw.URL)
	agent.tick() // Should not panic
}

func TestFetchConfig_EmptyConfigText(t *testing.T) {
	// Gateway returns a config hash but empty config text
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		action := r.FormValue("Action")
		w.Header().Set("Content-Type", "text/xml")
		switch action {
		case "LBAgentHeartbeat":
			fmt.Fprintf(w, `<LBAgentHeartbeatResponse><LBAgentHeartbeatResult><Status>active</Status><ConfigHash>hash-x</ConfigHash></LBAgentHeartbeatResult></LBAgentHeartbeatResponse>`)
		case "GetLBConfig":
			fmt.Fprintf(w, `<GetLBConfigResponse><GetLBConfigResult><ConfigText></ConfigText><ConfigHash>hash-x</ConfigHash></GetLBConfigResult></GetLBConfigResponse>`)
		}
	}))
	defer gw.Close()

	agent := newTestAgent(t, gw.URL)
	agent.tick() // Should handle empty config gracefully

	// Config should NOT be updated
	if agent.localConfigHash != "" {
		t.Errorf("localConfigHash should be empty, got %q", agent.localConfigHash)
	}
}

func TestFetchConfig_GatewayErrorOnGetConfig(t *testing.T) {
	callCount := 0
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		action := r.FormValue("Action")
		w.Header().Set("Content-Type", "text/xml")
		switch action {
		case "LBAgentHeartbeat":
			fmt.Fprintf(w, `<LBAgentHeartbeatResponse><LBAgentHeartbeatResult><Status>active</Status><ConfigHash>hash-new</ConfigHash></LBAgentHeartbeatResult></LBAgentHeartbeatResponse>`)
		case "GetLBConfig":
			callCount++
			http.Error(w, "server error", http.StatusInternalServerError)
		}
	}))
	defer gw.Close()

	agent := newTestAgent(t, gw.URL)
	agent.tick()

	if callCount != 1 {
		t.Errorf("GetLBConfig called %d times, want 1", callCount)
	}
	if agent.localConfigHash != "" {
		t.Errorf("localConfigHash should be empty after fetch failure, got %q", agent.localConfigHash)
	}
}

func TestSendHeartbeat_MultipleServers(t *testing.T) {
	var serverCount int
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		// Count server entries
		for i := 1; ; i++ {
			if r.FormValue(fmt.Sprintf("Servers.member.%d.Backend", i)) == "" {
				serverCount = i - 1
				break
			}
		}
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprintf(w, `<LBAgentHeartbeatResponse><LBAgentHeartbeatResult><Status>active</Status><ConfigHash>h1</ConfigHash></LBAgentHeartbeatResult></LBAgentHeartbeatResponse>`)
	}))
	defer gw.Close()

	agent := newTestAgent(t, gw.URL)
	agent.localConfigHash = "h1"
	agent.statsFn = func(_ string) ([]ServerStatus, error) {
		return []ServerStatus{
			{Backend: "bk1", Server: "srv1", Status: "UP"},
			{Backend: "bk1", Server: "srv2", Status: "DOWN"},
			{Backend: "bk2", Server: "srv3", Status: "UP"},
		}, nil
	}

	agent.tick()

	if serverCount != 3 {
		t.Errorf("server count = %d, want 3", serverCount)
	}
}

func TestFetchConfig_WriteError(t *testing.T) {
	configText := "global\n"
	gw := fakeGateway(t, "hash-new", configText, "active")
	defer gw.Close()

	agent := newTestAgent(t, gw.URL)
	// Point configPath to a read-only directory
	agent.configPath = "/proc/nonexistent/haproxy.cfg"

	agent.tick()

	if agent.localConfigHash != "" {
		t.Errorf("localConfigHash should be empty after write failure, got %q", agent.localConfigHash)
	}
}
