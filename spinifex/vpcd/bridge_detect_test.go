//test:in-package — detectBridgeMode, resolveBridgeConfig and the ifaceExists
// probe they read are all unexported.

package vpcd

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/host"
)

// captureLogs redirects slog for one test and returns the accumulated output.
// The detection's whole job is to say what it saw, so the log line is the
// behaviour under test and not incidental output.
func captureLogs(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf.String
}

// A wrong verdict here misroutes every external packet, and the previous log
// line named only the branch it took — so a node detected as veth when it was
// wired for nat looked identical to one that was right.
func TestDetectBridgeModeReportsBothProbes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		links []string
		want  string
	}{
		{"routed NAT transit veth", []string{host.NATTransitOVSEnd}, BridgeModeNAT},
		{"Linux bridge linked by veth", []string{wanVethOVSEnd}, BridgeModeVeth},
		{"WAN NIC is an OVS port", nil, BridgeModeDirect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubDetectProbes(t, tc.links)
			logs := captureLogs(t)

			if got := detectBridgeMode("enp0s3", ""); got != tc.want {
				t.Errorf("mode = %q, want %q", got, tc.want)
			}
			out := logs()
			for _, probe := range []string{host.NATTransitOVSEnd, wanVethOVSEnd} {
				if !strings.Contains(out, probe) {
					t.Errorf("log does not report the %s probe: %s", probe, out)
				}
			}
		})
	}
}

// A node set up twice in two modes carries both veths. Picking one silently is
// how it stays broken; --teardown is the thing to reach for, so the log says so.
func TestDetectBridgeModeFlagsANodeWiredTwice(t *testing.T) {
	stubDetectProbes(t, []string{host.NATTransitOVSEnd, wanVethOVSEnd})
	logs := captureLogs(t)

	if got := detectBridgeMode("enp0s3", ""); got != BridgeModeNAT {
		t.Errorf("mode = %q, want %q — nat must still win so behaviour is unchanged", got, BridgeModeNAT)
	}
	out := logs()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "--teardown") {
		t.Errorf("both veths present must log an error naming --teardown: %s", out)
	}
}

// The wiring is what the datapath uses, so external_mode losing the argument is
// correct — but silently is not. This is the shape that had a cluster running
// nat config on pool wiring with every signal green.
func TestDetectBridgeModeFlagsAConfigThatDisagreesWithTheWiring(t *testing.T) {
	for _, tc := range []struct {
		name         string
		links        []string
		externalMode string
		wantErrorLog bool
	}{
		{"nat asked for, nat wired", []string{host.NATTransitOVSEnd}, "nat", false},
		{"nat asked for, veth wired", []string{wanVethOVSEnd}, "nat", true},
		{"pool asked for, nat wired", []string{host.NATTransitOVSEnd}, "pool", true},
		{"pool asked for, veth wired", []string{wanVethOVSEnd}, "pool", false},
		{"no external mode is not checked", []string{host.NATTransitOVSEnd}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubDetectProbes(t, tc.links)
			logs := captureLogs(t)

			detectBridgeMode("enp0s3", tc.externalMode)
			logged := strings.Contains(logs(), "external_mode and the uplink wiring disagree")
			if logged != tc.wantErrorLog {
				t.Errorf("disagreement logged = %v, want %v: %s", logged, tc.wantErrorLog, logs())
			}
		})
	}
}

// An explicitly configured bridge_mode is obeyed, and is exactly as capable of
// being wrong as a guess — nothing else ever checks it against the wiring.
func TestResolveBridgeConfigFlagsAnExplicitModeThatDoesNotMatch(t *testing.T) {
	stubDetectProbes(t, []string{wanVethOVSEnd})
	logs := captureLogs(t)

	mode, br := resolveBridgeConfig(BridgeModeNAT, "enp0s3", "nat")
	if mode != BridgeModeNAT || br != host.NATTransitHostEnd {
		t.Errorf("got (%q,%q), want (%q,%q) — the configured value is still obeyed",
			mode, br, BridgeModeNAT, host.NATTransitHostEnd)
	}
	if !strings.Contains(logs(), "configured bridge_mode does not match the uplink wiring") {
		t.Errorf("a configured mode contradicting the wiring must be reported: %s", logs())
	}
}
