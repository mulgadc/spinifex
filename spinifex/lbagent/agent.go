// Package lbagent implements the LB config agent that runs inside load balancer VMs.
// It polls the AWS gateway for config updates and reports health via heartbeats.
// All communication uses SigV4-signed HTTP requests to the gateway.
package lbagent

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mulgadc/predastore/auth"
	"github.com/mulgadc/spinifex/internal/tlsconfig"
)

const (
	// Default paths for HAProxy config and PID file.
	DefaultConfigPath = "/etc/haproxy/haproxy.cfg"
	DefaultPIDPath    = "/run/haproxy.pid"

	// CertDir holds TLS PEM files for HTTPS listeners (0600). Delivered paths
	// are constrained to this dir to prevent traversal writes.
	CertDir = "/etc/haproxy/certs"

	// Nginx paths for the NLB (L4) data plane. ALBs use the HAProxy paths above.
	NginxConfigPath = "/etc/nginx/nginx.conf"
	NginxPIDPath    = "/run/nginx.pid"
	NginxCertDir    = "/etc/nginx/certs"

	// pollInterval is how often the agent sends a heartbeat.
	pollInterval = 5 * time.Second
)

// Data-plane engine names (duplicated from handlers/elbv2 to avoid an import cycle).
const (
	EngineHAProxy = "haproxy"
	EngineNginx   = "nginx"
)

// Agent manages HAProxy configuration inside an LB VM by polling the gateway.
type Agent struct {
	lbID       string
	gatewayURL string
	region     string // SigV4 signing region
	configPath string
	pidPath    string
	certDir    string // dir for TLS cert PEM files; defaults to CertDir (overridable in tests)
	socketPath string // HAProxy stats socket

	accessKey string
	secretKey string
	client    *http.Client

	localConfigHash string
	engine          string         // data-plane engine of the last applied config
	healthTargets   []HealthTarget // active probe targets (nginx/NLB only)
	stopCh          chan struct{}

	// For testing: override the reload functions (HAProxy / nginx).
	reloadFn      func(configPath, pidPath string) error
	reloadNginxFn func(configPath, pidPath string) error
	// For testing: override the stats query function.
	statsFn func(socketPath string) ([]ServerStatus, error)
	// For testing: override the active health prober (nginx/NLB).
	probeFn func(targets []HealthTarget) []ServerStatus
}

// New creates a new LB agent for the given load balancer.
func New(lbID, gatewayURL, accessKey, secretKey, region string) (*Agent, error) {
	if lbID == "" {
		return nil, fmt.Errorf("lbID is required")
	}
	if gatewayURL == "" {
		return nil, fmt.Errorf("gatewayURL is required")
	}
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("access key and secret key are required")
	}
	if region == "" {
		return nil, fmt.Errorf("region is required")
	}

	// Use system CA trust store (CA cert injected via cloud-init ca_certs).
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:       tls.VersionTLS13,
				CurvePreferences: tlsconfig.Curves,
			},
			MaxIdleConns:    2,
			IdleConnTimeout: 30 * time.Second,
		},
	}

	return &Agent{
		lbID:          lbID,
		gatewayURL:    strings.TrimRight(gatewayURL, "/"),
		region:        region,
		configPath:    DefaultConfigPath,
		pidPath:       DefaultPIDPath,
		certDir:       CertDir,
		socketPath:    fmt.Sprintf("/tmp/spinifex-haproxy/%s.sock", lbID),
		accessKey:     accessKey,
		secretKey:     secretKey,
		client:        client,
		stopCh:        make(chan struct{}),
		reloadFn:      reloadHAProxy,
		reloadNginxFn: reloadNginx,
		statsFn:       queryHAProxyStats,
		probeFn:       probeHealthTargets,
	}, nil
}

// Start runs the poll loop. It blocks until Stop is called.
func (a *Agent) Start() error {
	slog.Info("Agent started", "lbId", a.lbID, "gateway", a.gatewayURL, "region", a.region)

	// Run first tick immediately.
	a.tick()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.stopCh:
			slog.Info("Agent stopped", "lbId", a.lbID)
			return nil
		case <-ticker.C:
			a.tick()
		}
	}
}

// Stop signals the poll loop to exit.
func (a *Agent) Stop() {
	select {
	case <-a.stopCh:
	default:
		close(a.stopCh)
	}
}

// tick runs one heartbeat cycle: send health, check config hash, fetch if changed.
func (a *Agent) tick() {
	// nginx (NLB) has no per-server stats socket — the agent actively probes
	// the delivered health targets instead of reading HAProxy stats.
	var servers []ServerStatus
	if a.engine == EngineNginx {
		servers = a.probeFn(a.healthTargets)
	} else {
		var statsErr error
		servers, statsErr = a.statsFn(a.socketPath)
		if statsErr != nil {
			slog.Warn("HAProxy stats unavailable", "err", statsErr)
		}
	}

	resp, err := a.sendHeartbeat(servers)
	if err != nil {
		// Include the dial target so the serial console disambiguates a
		// transport failure (never reached AWSGW — "send request: dial ...")
		// from an HTTP-level rejection ("gateway returned NNN: ...").
		slog.Error("Heartbeat failed", "err", err, "gateway", a.gatewayURL, "region", a.region)
		return
	}

	slog.Debug("Heartbeat OK", "status", resp.Status, "configHash", resp.ConfigHash)

	if resp.ConfigHash != "" && resp.ConfigHash != a.localConfigHash {
		slog.Info("Config hash changed", "remote", resp.ConfigHash, "local", a.localConfigHash)
		if err := a.fetchAndApplyConfig(); err != nil {
			slog.Error("Config update failed", "err", err)
			return
		}
	}
}

// heartbeatResponse is the parsed XML response from LBAgentHeartbeat.
type heartbeatResponse struct {
	XMLName    xml.Name `xml:"LBAgentHeartbeatResponse"`
	Status     string   `xml:"LBAgentHeartbeatResult>Status"`
	ConfigHash string   `xml:"LBAgentHeartbeatResult>ConfigHash"`
}

// certFile is one delivered TLS certificate PEM parsed from the GetLBConfig
// response. Path is the absolute destination under CertDir.
type certFile struct {
	Path string `xml:"Path"`
	PEM  string `xml:"PEM"`
}

// configResponse is the parsed XML response from GetLBConfig.
type configResponse struct {
	XMLName       xml.Name       `xml:"GetLBConfigResponse"`
	ConfigText    string         `xml:"GetLBConfigResult>ConfigText"`
	ConfigHash    string         `xml:"GetLBConfigResult>ConfigHash"`
	Engine        string         `xml:"GetLBConfigResult>Engine"`
	CertFiles     []certFile     `xml:"GetLBConfigResult>CertFiles>member"`
	HealthTargets []HealthTarget `xml:"GetLBConfigResult>HealthTargets>member"`
}

// enginePaths returns the data-plane file paths and reload function for the
// given engine, defaulting to HAProxy when the gateway omits Engine.
func (a *Agent) enginePaths(engine string) (configPath, pidPath, certDir string, reload func(string, string) error) {
	if engine == EngineNginx {
		return NginxConfigPath, NginxPIDPath, NginxCertDir, a.reloadNginxFn
	}
	return a.configPath, a.pidPath, a.certDir, a.reloadFn
}

// sendHeartbeat sends a heartbeat with health report to the gateway.
func (a *Agent) sendHeartbeat(servers []ServerStatus) (*heartbeatResponse, error) {
	params := url.Values{}
	params.Set("Action", "LBAgentHeartbeat")
	params.Set("Version", "2015-12-01")
	params.Set("LBID", a.lbID)

	for i, s := range servers {
		idx := strconv.Itoa(i + 1)
		params.Set("Servers.member."+idx+".Backend", s.Backend)
		params.Set("Servers.member."+idx+".Server", s.Server)
		params.Set("Servers.member."+idx+".Status", s.Status)
	}

	body, err := a.signedPost(params)
	if err != nil {
		return nil, fmt.Errorf("heartbeat: %w", err)
	}

	var resp heartbeatResponse
	if err := xml.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse heartbeat response: %w", err)
	}
	return &resp, nil
}

// fetchAndApplyConfig fetches the current config from the gateway and applies it.
func (a *Agent) fetchAndApplyConfig() error {
	params := url.Values{}
	params.Set("Action", "GetLBConfig")
	params.Set("Version", "2015-12-01")
	params.Set("LBID", a.lbID)

	body, err := a.signedPost(params)
	if err != nil {
		return fmt.Errorf("get config: %w", err)
	}

	var resp configResponse
	if err := xml.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse config response: %w", err)
	}

	if resp.ConfigText == "" {
		return fmt.Errorf("empty config returned")
	}

	engine := resp.Engine
	if engine == "" {
		engine = EngineHAProxy
	}
	configPath, pidPath, certDir, reload := a.enginePaths(engine)

	// Cert files must land before the config that references them (via
	// `ssl crt` / `ssl_certificate`), otherwise the engine fails to
	// start/reload on a missing path.
	if err := a.writeCertFiles(certDir, resp.CertFiles); err != nil {
		return fmt.Errorf("write cert files: %w", err)
	}

	if err := WriteConfig(configPath, resp.ConfigText); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	if err := reload(configPath, pidPath); err != nil {
		return fmt.Errorf("reload %s: %w", engine, err)
	}

	a.engine = engine
	a.healthTargets = resp.HealthTargets
	a.localConfigHash = resp.ConfigHash
	slog.Info("Config applied", "engine", engine, "hash", resp.ConfigHash, "healthTargets", len(resp.HealthTargets))
	return nil
}

// signedPost sends a SigV4-signed POST to the gateway and returns the response body.
func (a *Agent) signedPost(params url.Values) ([]byte, error) {
	body := params.Encode()
	req, err := http.NewRequest(http.MethodPost, a.gatewayURL, bytes.NewReader([]byte(body)))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	sum := sha256.Sum256([]byte(body))
	payloadHash := hex.EncodeToString(sum[:])
	if err := auth.SignReq(req, a.accessKey, a.secretKey, payloadHash, "elasticloadbalancing", a.region); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway returned %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// writeCertFiles writes each delivered TLS PEM to its path (0600) under certDir.
// Paths that escape certDir are rejected to prevent traversal writes.
func (a *Agent) writeCertFiles(certDir string, certs []certFile) error {
	if len(certs) == 0 {
		return nil
	}
	if err := os.MkdirAll(certDir, 0o750); err != nil {
		return fmt.Errorf("create cert dir: %w", err)
	}
	for _, c := range certs {
		clean := filepath.Clean(c.Path)
		if clean != filepath.Join(certDir, filepath.Base(clean)) {
			return fmt.Errorf("cert path %q escapes %s", c.Path, certDir)
		}
		if c.PEM == "" {
			return fmt.Errorf("cert %q has empty PEM", c.Path)
		}
		if err := os.WriteFile(clean, []byte(c.PEM), 0o600); err != nil {
			return fmt.Errorf("write cert %q: %w", clean, err)
		}
		slog.Info("Wrote TLS cert", "path", clean, "bytes", len(c.PEM))
	}
	return nil
}

// WriteConfig atomically writes an HAProxy config file.
// It writes to a temp file first, then renames for atomicity.
func WriteConfig(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename config: %w", err)
	}
	return nil
}

// reloadHAProxy starts or reloads the HAProxy process.
// If HAProxy is running (PID file exists and process alive), it does a
// graceful reload with -sf. Otherwise it starts a fresh instance.
func reloadHAProxy(configPath, pidPath string) error {
	// Ensure the stats socket directory exists (the config may reference
	// /tmp/spinifex-haproxy/ which doesn't exist on fresh Alpine VMs).
	_ = os.MkdirAll("/tmp/spinifex-haproxy", 0o750)

	oldPID := readPID(pidPath)

	var cmd *exec.Cmd
	if oldPID > 0 {
		// Graceful reload: new worker replaces old in-flight requests.
		cmd = exec.Command("haproxy", "-f", configPath, "-p", pidPath, "-D", "-sf", strconv.Itoa(oldPID))
	} else {
		cmd = exec.Command("haproxy", "-f", configPath, "-p", pidPath, "-D")
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("haproxy: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// reloadNginx validates then starts or reloads nginx for the NLB data plane.
// `nginx -t` fails closed before any reload (a bad config never replaces a good one).
func reloadNginx(configPath, pidPath string) error {
	// Pre-create runtime dirs; stripped Alpine microvms may not have them.
	for _, dir := range []string{filepath.Dir(pidPath), "/var/lib/nginx", "/var/lib/nginx/tmp", "/var/log/nginx"} {
		_ = os.MkdirAll(dir, 0o750)
	}

	test := exec.Command("nginx", "-t", "-c", configPath)
	if out, err := test.CombinedOutput(); err != nil {
		return fmt.Errorf("nginx -t: %s: %w", strings.TrimSpace(string(out)), err)
	}

	var cmd *exec.Cmd
	if readPID(pidPath) > 0 {
		cmd = exec.Command("nginx", "-s", "reload", "-c", configPath)
	} else {
		cmd = exec.Command("nginx", "-c", configPath)
	}

	// nginx daemonizes and inherits stderr, so CombinedOutput blocks forever.
	// Capture to a temp file; Wait returns once the foreground launcher exits.
	logFile, err := os.CreateTemp("", "nginx-reload-*.log")
	if err != nil {
		return fmt.Errorf("create nginx log: %w", err)
	}
	defer os.Remove(logFile.Name())
	defer logFile.Close()
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Run(); err != nil {
		out, _ := os.ReadFile(logFile.Name())
		return fmt.Errorf("nginx: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// readPID reads the HAProxy PID from the PID file. Returns 0 if unavailable.
func readPID(pidPath string) int {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	// Check if process is still alive
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return 0
	}
	return pid
}
