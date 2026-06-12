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

// HealthTarget is a backend the agent actively health-checks for the nginx NLB
// data plane (OSS nginx stream has no built-in upstream probing).
type HealthTarget struct {
	ServerName string `xml:"ServerName"`
	Address    string `xml:"Address"`  // ip:port to probe
	Protocol   string `xml:"Protocol"` // TCP | HTTP | HTTPS
	Path       string `xml:"Path"`     // HTTP(S) path
}

// probeTimeout bounds each individual target probe.
const probeTimeout = 3 * time.Second

// probeHealthTargets probes each health target and returns one ServerStatus per
// target. An empty list returns nil.
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

// probeOne performs a single health check. TCP: completed dial. HTTP/HTTPS:
// status < 400 (HTTPS skips cert verification, matching AWS NLB behavior).
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
