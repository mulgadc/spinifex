package host

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestIMDSBridgeIsNotBrInt locks in the coexistence decision: the IMDS redirect
// flows live on a dedicated bridge, never br-int. ovn-controller flushes foreign
// br-int flows on restart, so installing them there would break IMDS on every
// ovn-controller lifecycle event.
func TestIMDSBridgeIsNotBrInt(t *testing.T) {
	if IMDSBridge == "br-int" {
		t.Fatalf("IMDSBridge must not be br-int: ovn-controller flushes foreign br-int flows on restart")
	}
	if IMDSBridge == "" {
		t.Fatal("IMDSBridge must be a non-empty bridge name")
	}
}

func TestEnsureIMDSBridge(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl", nil, nil)
	s.expect("ip", nil, nil)

	if err := EnsureIMDSBridge(context.Background(), s); err != nil {
		t.Fatalf("EnsureIMDSBridge: %v", err)
	}

	for _, want := range []string{
		"ovs-vsctl --may-exist add-br " + IMDSBridge,
		"ovs-vsctl set Bridge " + IMDSBridge + " fail-mode=secure",
		"ip link set " + IMDSBridge + " up",
	} {
		if !s.called(want) {
			t.Errorf("expected command %q; calls: %v", want, s.calls)
		}
	}

	// Coexistence guard: EnsureIMDSBridge must never touch br-int and must never
	// register with OVN, or ovn-controller would adopt and flush the bridge.
	for _, c := range s.calls {
		if strings.Contains(c, "br-int") {
			t.Errorf("EnsureIMDSBridge must not touch br-int: %q", c)
		}
		if strings.Contains(c, "ovn-bridge-mappings") || strings.Contains(c, "external_ids:ovn") {
			t.Errorf("EnsureIMDSBridge must not register the bridge with OVN: %q", c)
		}
	}
}

func TestEnsureIMDSBridgeAddBrError(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl --may-exist add-br", nil, errors.New("boom"))

	err := EnsureIMDSBridge(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), IMDSBridge) {
		t.Fatalf("expected error naming %s, got %v", IMDSBridge, err)
	}
}

func TestRemoveIMDSBridge(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl", nil, nil)

	if err := RemoveIMDSBridge(context.Background(), s); err != nil {
		t.Fatalf("RemoveIMDSBridge: %v", err)
	}
	if !s.called("ovs-vsctl --if-exists del-br " + IMDSBridge) {
		t.Errorf("expected del-br; calls: %v", s.calls)
	}
}

func TestInstallIMDSFlow(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-ofctl", nil, nil)

	spec := "table=0,priority=200,in_port=7,actions=drop"
	if err := installIMDSFlow(context.Background(), s, spec); err != nil {
		t.Fatalf("installIMDSFlow: %v", err)
	}

	want := "ovs-ofctl add-flow " + IMDSBridge + " cookie=" + imdsFlowCookie + "," + spec
	if !s.called(want) {
		t.Errorf("expected %q; calls: %v", want, s.calls)
	}
}

func TestClearIMDSFlows(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-ofctl", nil, nil)

	if err := clearIMDSFlows(context.Background(), s); err != nil {
		t.Fatalf("clearIMDSFlows: %v", err)
	}
	// Cookie mask /-1 deletes only the IMDS flow-group, never another tenant's flows.
	want := "ovs-ofctl del-flows " + IMDSBridge + " cookie=" + imdsFlowCookie + "/-1"
	if !s.called(want) {
		t.Errorf("expected cookie-scoped del-flows; calls: %v", s.calls)
	}
}
