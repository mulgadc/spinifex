// Package nettest runs one direction of a node-to-node throughput test with
// iperf2, bound to one plane's address on each end.
package nettest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
)

// Limits on one direction of a test. A request outside them is refused.
const (
	MinDuration = 5
	MaxDuration = 30
	MaxStreams  = 16
)

const (
	serverGrace  = 15 * time.Second
	readyTimeout = 3 * time.Second
	pingCount    = 10
)

var (
	iperfPaths   = []string{"/usr/bin/iperf", "/usr/local/bin/iperf"}
	setprivPaths = []string{"/usr/bin/setpriv", "/bin/setpriv"}
	pingPaths    = []string{"/usr/bin/ping", "/bin/ping"}
)

func findTool(paths []string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// ErrNoIperf is returned on a node without iperf2 installed.
var ErrNoIperf = errors.New("iperf (version 2) is not installed on this node")

// unprivileged wraps a command so it runs with no capabilities. The daemon's
// ambient capabilities would otherwise pass to a network-facing listener.
func unprivileged(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	setpriv := findTool(setprivPaths)
	if setpriv == "" {
		return nil, errors.New("setpriv is not installed, refusing to run a listener with the daemon's capabilities")
	}
	full := append([]string{"--inh-caps=-all", "--ambient-caps=-all", "--no-new-privs", name}, args...)
	cmd := exec.CommandContext(ctx, setpriv, full...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd, nil
}

// Server is one listener; only one runs on a node at a time.
type Server struct {
	mu   sync.Mutex
	stop context.CancelFunc
}

// Serve starts a one-shot iperf server on bindIP that exits after the test
// window, and returns once it accepts connections.
func (s *Server) Serve(bindIP string, durationSeconds int) error {
	iperf := findTool(iperfPaths)
	if iperf == "" {
		return ErrNoIperf
	}
	if net.ParseIP(bindIP) == nil {
		return fmt.Errorf("invalid bind address %q", bindIP)
	}
	window := time.Duration(durationSeconds)*time.Second + serverGrace

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		s.stop()
		s.stop = nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), window)
	cmd, err := unprivileged(ctx, iperf, "-s", "-B", bindIP, "-p", strconv.Itoa(clusterv1.NetTestPort),
		"-t", strconv.Itoa(int(window.Seconds())))
	if err != nil {
		cancel()
		return err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start iperf server: %w", err)
	}
	s.stop = cancel
	go func() {
		_ = cmd.Wait()
		cancel()
	}()

	addr := net.JoinHostPort(bindIP, strconv.Itoa(clusterv1.NetTestPort))
	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("iperf server exited before it was ready")
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("iperf server did not start listening")
}

// Stop ends a running listener, if any.
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		s.stop()
		s.stop = nil
	}
}

// Run is the client side: it sends to address from bindIP for the duration
// and measures the round trip with ping beforehand, so the two never overlap.
func Run(ctx context.Context, req clusterv1.NetTestRunRequest, bindIP string) (clusterv1.NetTestRunResponse, error) {
	out := clusterv1.NetTestRunResponse{SourceAddress: bindIP, Streams: req.Streams, Tool: "iperf2"}
	iperf := findTool(iperfPaths)
	if iperf == "" {
		return out, ErrNoIperf
	}
	if net.ParseIP(req.Address) == nil || net.ParseIP(bindIP) == nil {
		return out, errors.New("invalid address")
	}

	if ping := findTool(pingPaths); ping != "" {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		cmd, err := unprivileged(pctx, ping, "-n", "-q", "-c", strconv.Itoa(pingCount), "-i", "0.2", "-W", "1", "-I", bindIP, req.Address)
		if err == nil {
			if data, err := cmd.Output(); err == nil || len(data) > 0 {
				out.RTTMinMillis, out.RTTAvgMillis, out.RTTMaxMillis, out.PacketLoss = parsePing(string(data))
			}
		}
		cancel()
	}

	cctx, cancel := context.WithTimeout(ctx, time.Duration(req.DurationSeconds)*time.Second+serverGrace)
	defer cancel()
	cmd, err := unprivileged(cctx, iperf, "-c", req.Address, "-B", bindIP, "-p", strconv.Itoa(req.Port),
		"-t", strconv.Itoa(req.DurationSeconds), "-P", strconv.Itoa(req.Streams), "-y", "C")
	if err != nil {
		return out, err
	}
	data, err := cmd.Output()
	if err != nil && len(data) == 0 {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return out, fmt.Errorf("iperf: %s", strings.TrimSpace(string(exit.Stderr)))
		}
		return out, fmt.Errorf("iperf: %w", err)
	}
	if err := parseIperfCSV(string(data), &out); err != nil {
		return out, err
	}
	return out, nil
}

// parseIperfCSV reads iperf2's `-y C` report. With more than one stream the
// line whose ID is -1 is the sum; with one, the only line is.
func parseIperfCSV(text string, out *clusterv1.NetTestRunResponse) error {
	var sum, single []string
	streams := 0
	for line := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) < 9 {
			continue
		}
		if f[5] == "-1" {
			sum = f
			continue
		}
		streams++
		single = f
		if bps, err := strconv.ParseFloat(f[8], 64); err == nil {
			out.StreamBPS = append(out.StreamBPS, bps)
		}
	}
	row := sum
	if row == nil {
		row = single
	}
	if row == nil {
		return errors.New("iperf printed no report")
	}
	out.Streams = streams
	out.Bytes, _ = strconv.ParseInt(row[7], 10, 64)
	out.BitsPerSecond, _ = strconv.ParseFloat(row[8], 64)
	if start, end, ok := strings.Cut(row[6], "-"); ok {
		s, _ := strconv.ParseFloat(start, 64)
		e, _ := strconv.ParseFloat(end, 64)
		out.Seconds = e - s
	}
	return nil
}

// parsePing reads "x% packet loss" and "rtt min/avg/max/mdev = a/b/c/d ms".
func parsePing(text string) (minMs, avgMs, maxMs, loss float64) {
	for line := range strings.SplitSeq(text, "\n") {
		if i := strings.Index(line, "% packet loss"); i > 0 {
			fields := strings.Fields(line[:i])
			if len(fields) > 0 {
				loss, _ = strconv.ParseFloat(fields[len(fields)-1], 64)
			}
		}
		if _, values, ok := strings.Cut(line, "= "); ok && strings.Contains(line, "min/avg/max") {
			parts := strings.Split(strings.Fields(values)[0], "/")
			if len(parts) >= 3 {
				minMs, _ = strconv.ParseFloat(parts[0], 64)
				avgMs, _ = strconv.ParseFloat(parts[1], 64)
				maxMs, _ = strconv.ParseFloat(parts[2], 64)
			}
		}
	}
	return minMs, avgMs, maxMs, loss
}
