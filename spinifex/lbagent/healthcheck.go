package lbagent

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// HealthTarget is one backend the agent must actively health-check, delivered
// by GetLBConfig for the nginx (NLB) data plane. OSS nginx `stream` has no
// active upstream probing, so the agent probes each target every tick and
// reports the result. ServerName is the daemon-computed name the health checker
// keys on — echoed back verbatim so the agent needs no ELBv2 naming logic.
type HealthTarget struct {
	ServerName string `xml:"ServerName"`
	Address    string `xml:"Address"`  // ip:port to probe
	Protocol   string `xml:"Protocol"` // TCP | HTTP | HTTPS
	Path       string `xml:"Path"`     // HTTP(S) path
}

// probeTimeout bounds each individual target probe.
const probeTimeout = 3 * time.Second

// probeHealthTargets actively probes every delivered health target and returns
// one ServerStatus per target (Status "UP"/"DOWN"). The Server field carries
// the daemon-supplied ServerName so the daemon health checker matches it
// directly. An empty target list yields a nil report (the daemon early-returns
// on empty reports, leaving health untouched).
func probeHealthTargets(targets []HealthTarget) []ServerStatus {
	if len(targets) == 0 {
		return nil
	}
	out := make([]ServerStatus, 0, len(targets))
	for _, t := range targets {
		status := "DOWN"
		if probeOne(t) {
			status = "UP"
		}
		out = append(out, ServerStatus{Server: t.ServerName, Status: status})
	}
	return out
}

// probeOne performs a single health check against one target. TCP succeeds on a
// completed dial; HTTP/HTTPS succeeds on a response status < 400 (HTTPS skips
// certificate verification, matching how AWS NLB HTTPS health checks treat the
// target's self-signed cert).
func probeOne(t HealthTarget) bool {
	switch strings.ToUpper(t.Protocol) {
	case "HTTP", "HTTPS":
		return probeHTTP(t)
	default: // TCP and anything else falls back to a connect probe
		conn, err := net.DialTimeout("tcp", t.Address, probeTimeout)
		if err != nil {
			slog.Warn("TCP probe failed", "addr", t.Address, "err", err)
			return false
		}
		_ = conn.Close()
		return true
	}
}

// probeHTTP issues a GET to the target's health-check path and reports whether
// the response status is below 400.
func probeHTTP(t HealthTarget) bool {
	scheme := "http"
	client := &http.Client{Timeout: probeTimeout}
	if strings.EqualFold(t.Protocol, "HTTPS") {
		scheme = "https"
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // target uses a self-signed cert; NLB HTTPS checks don't verify it
		}
	}

	path := t.Path
	if path == "" {
		path = "/"
	} else if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	url := fmt.Sprintf("%s://%s%s", scheme, t.Address, path)
	resp, err := client.Get(url) //nolint:noctx // short fixed-timeout probe
	if err != nil {
		slog.Warn("HTTP probe failed", "url", url, "err", err)
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 400
}
