package vpcd

import (
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/host"
)

func TestVerifyBridgeMode_NATOK(t *testing.T) {
	stubBridgeProbes(t, map[string]string{host.NATTransitOVSEnd: OvnExternalBridge}, nil)
	stubDetectProbes(t, []string{host.NATTransitHostEnd})
	if err := verifyBridgeMode(BridgeModeNAT, "", host.NATTransitHostEnd); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestVerifyBridgeMode_NATMissingOvsPort(t *testing.T) {
	stubBridgeProbes(t, map[string]string{}, nil)
	stubDetectProbes(t, []string{host.NATTransitHostEnd})
	err := verifyBridgeMode(BridgeModeNAT, "", host.NATTransitHostEnd)
	if err == nil || !strings.Contains(err.Error(), "--nat-uplink") {
		t.Fatalf("expected setup-ovn.sh hint, got: %v", err)
	}
}

func TestVerifyBridgeMode_NATWrongOvsBridge(t *testing.T) {
	stubBridgeProbes(t, map[string]string{host.NATTransitOVSEnd: "br-wan"}, nil)
	stubDetectProbes(t, []string{host.NATTransitHostEnd})
	err := verifyBridgeMode(BridgeModeNAT, "", host.NATTransitHostEnd)
	if err == nil || !strings.Contains(err.Error(), OvnExternalBridge) {
		t.Fatalf("expected br-ext mismatch, got: %v", err)
	}
}

func TestVerifyBridgeMode_NATMissingHostEnd(t *testing.T) {
	stubBridgeProbes(t, map[string]string{host.NATTransitOVSEnd: OvnExternalBridge}, nil)
	stubDetectProbes(t, nil)
	err := verifyBridgeMode(BridgeModeNAT, "", host.NATTransitHostEnd)
	if err == nil || !strings.Contains(err.Error(), host.NATTransitHostEnd) {
		t.Fatalf("expected missing host end error, got: %v", err)
	}
}

func TestVerifyBridgeMode_UnknownModeListsNAT(t *testing.T) {
	err := verifyBridgeMode("bogus", "", "")
	if err == nil || !strings.Contains(err.Error(), BridgeModeNAT) {
		t.Fatalf("expected error listing nat mode, got: %v", err)
	}
}

func TestDetectBridgeMode_NATWinsOverVeth(t *testing.T) {
	stubDetectProbes(t, []string{host.NATTransitOVSEnd, "veth-wan-ovs"})
	if got := detectBridgeMode("enp0s3"); got != BridgeModeNAT {
		t.Errorf("want %q, got %q", BridgeModeNAT, got)
	}
}

func TestResolveBridgeConfig_NATUsesTransitHostEnd(t *testing.T) {
	mode, br := resolveBridgeConfig(BridgeModeNAT, "")
	if mode != BridgeModeNAT || br != host.NATTransitHostEnd {
		t.Errorf("got (%q,%q), want (%q,%q)", mode, br, BridgeModeNAT, host.NATTransitHostEnd)
	}
}
