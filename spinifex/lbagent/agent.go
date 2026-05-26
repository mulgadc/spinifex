// Package lbagent implements the LB config agent that runs inside load balancer VMs.
// It polls the AWS gateway for config updates and reports health via heartbeats.
// All communication uses SigV4-signed HTTP requests to the gateway.
package lbagent

import (
	"bytes"
	"crypto/tls"
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

	"github.com/aws/aws-sdk-go/aws/credentials"
	v4 "github.com/aws/aws-sdk-go/aws/signer/v4"
	"github.com/mulgadc/spinifex/internal/tlsconfig"
)

const (
	// Default paths for HAProxy config and PID file.
	DefaultConfigPath = "/etc/haproxy/haproxy.cfg"
	DefaultPIDPath    = "/run/haproxy.pid"

	// pollInterval is how often the agent sends a heartbeat.
	pollInterval = 5 * time.Second
)

// Agent manages HAProxy configuration inside an LB VM by polling the gateway.
type Agent struct {
	lbID       string
	gatewayURL string
	region     string // SigV4 signing region
	configPath string
	pidPath    string
	socketPath string // HAProxy stats socket

	signer *v4.Signer
	client *http.Client

	localConfigHash string
	stopCh          chan struct{}

	// For testing: override the reload function.
	reloadFn func(configPath, pidPath string) error
	// For testing: override the stats query function.
	statsFn func(socketPath string) ([]ServerStatus, error)
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

	creds := credentials.NewStaticCredentials(accessKey, secretKey, "")
	signer := v4.NewSigner(creds)

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
		lbID:       lbID,
		gatewayURL: strings.TrimRight(gatewayURL, "/"),
		region:     region,
		configPath: DefaultConfigPath,
		pidPath:    DefaultPIDPath,
		socketPath: fmt.Sprintf("/tmp/spinifex-haproxy/lb-%s.sock", lbID),
		signer:     signer,
		client:     client,
		stopCh:     make(chan struct{}),
		reloadFn:   reloadHAProxy,
		statsFn:    queryHAProxyStats,
	}, nil
}

// Start runs the poll loop. It blocks until Stop is called.
func (a *Agent) Start() error {
	slog.Info("Agent started", "lbId", a.lbID, "gateway", a.gatewayURL)

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
	servers, err := a.statsFn(a.socketPath)
	if err != nil {
		slog.Warn("HAProxy stats unavailable", "err", err)
	}

	resp, err := a.sendHeartbeat(servers)
	if err != nil {
		slog.Error("Heartbeat failed", "err", err)
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

// configResponse is the parsed XML response from GetLBConfig.
type configResponse struct {
	XMLName    xml.Name `xml:"GetLBConfigResponse"`
	ConfigText string   `xml:"GetLBConfigResult>ConfigText"`
	ConfigHash string   `xml:"GetLBConfigResult>ConfigHash"`
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

	if err := WriteConfig(a.configPath, resp.ConfigText); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	if err := a.reloadFn(a.configPath, a.pidPath); err != nil {
		return fmt.Errorf("reload haproxy: %w", err)
	}

	a.localConfigHash = resp.ConfigHash
	slog.Info("Config applied", "hash", resp.ConfigHash)
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

	_, err = a.signer.Sign(req, bytes.NewReader([]byte(body)), "elasticloadbalancing", a.region, time.Now())
	if err != nil {
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
		// Graceful reload: new worker starts, old workers finish in-flight requests
		cmd = exec.Command("haproxy", "-f", configPath, "-p", pidPath, "-D", "-sf", strconv.Itoa(oldPID))
	} else {
		// Fresh start
		cmd = exec.Command("haproxy", "-f", configPath, "-p", pidPath, "-D")
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("haproxy: %s: %w", strings.TrimSpace(string(out)), err)
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
