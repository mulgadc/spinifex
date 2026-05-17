//go:build e2e

package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TrafficResult summarises a round of probes against an LB target.
type TrafficResult struct {
	Total        int
	Successful   int
	Distribution map[string]int // responder ID -> count
}

// Unique returns the number of distinct responders observed.
func (r TrafficResult) Unique() int { return len(r.Distribution) }

// HTTPRoundRobin sends n GET requests to url, parses each response body as
// JSON {"instance_id":"..."}, and counts responders. Matches the app-userdata
// HTTP responder in run-lb-e2e.sh.
func HTTPRoundRobin(url string, n int, timeout time.Duration) TrafficResult {
	r := TrafficResult{Distribution: map[string]int{}, Total: n}
	client := &http.Client{Timeout: timeout}
	for i := 0; i < n; i++ {
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		var payload struct {
			InstanceID string `json:"instance_id"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || payload.InstanceID == "" {
			continue
		}
		r.Distribution[payload.InstanceID]++
		r.Successful++
	}
	return r
}

// TCPRoundRobin opens n TCP connections to host:port, reads a line per conn,
// and counts responders. Matches the app-userdata TCP echo responder.
func TCPRoundRobin(host string, port int, n int, timeout time.Duration) TrafficResult {
	r := TrafficResult{Distribution: map[string]int{}, Total: n}
	for i := 0; i < n; i++ {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), timeout)
		if err != nil {
			continue
		}
		conn.SetReadDeadline(time.Now().Add(timeout))
		buf := make([]byte, 256)
		nb, err := conn.Read(buf)
		conn.Close()
		if err != nil && err != io.EOF {
			continue
		}
		resp := strings.TrimSpace(string(buf[:nb]))
		if resp == "" {
			continue
		}
		r.Distribution[resp]++
		r.Successful++
	}
	return r
}

// AssertRoundRobin asserts that r has at least minUnique distinct responders
// and at least minSuccess successful probes. Logs the distribution either way.
func AssertRoundRobin(t *testing.T, r TrafficResult, minUnique, minSuccess int, label string) {
	t.Helper()
	for inst, count := range r.Distribution {
		t.Logf("  %s: %s -> %d responses", label, inst, count)
	}
	t.Logf("  %s: %d/%d successful, %d unique", label, r.Successful, r.Total, r.Unique())
	if r.Unique() < minUnique {
		t.Errorf("%s: want >=%d unique responders, got %d", label, minUnique, r.Unique())
	}
	if r.Successful < minSuccess {
		t.Errorf("%s: want >=%d successful, got %d/%d", label, minSuccess, r.Successful, r.Total)
	}
}

// VerifyResultsLines parses newline-separated probe results captured on a
// remote client VM and returns the distribution. Used when the client wrote
// curl/nc output to a file we then fetched.
func VerifyResultsLines(raw, proto string) TrafficResult {
	r := TrafficResult{Distribution: map[string]int{}}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		r.Total++
		var id string
		if proto == "http" {
			var payload struct {
				InstanceID string `json:"instance_id"`
			}
			if err := json.Unmarshal([]byte(line), &payload); err != nil || payload.InstanceID == "" {
				continue
			}
			id = payload.InstanceID
		} else {
			id = line
		}
		r.Distribution[id]++
		r.Successful++
	}
	return r
}
