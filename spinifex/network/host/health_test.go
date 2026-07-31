package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

const (
	brExistsCmd     = "ovs-vsctl br-exists br-int"
	connStatusCmd   = "ovn-appctl -t ovn-controller connection-status"
	systemIDCmd     = "ovs-vsctl get Open_vSwitch . external_ids:system-id"
	encapIPCmd      = "ovs-vsctl get Open_vSwitch . external_ids:ovn-encap-ip"
	ovnRemoteCmd    = "ovs-vsctl get Open_vSwitch . external_ids:ovn-remote"
	healthyChassis  = "hydrogen"
	healthyEncapIP  = "192.168.1.13"
	healthyOVNRemte = "tcp:192.168.1.13:6642"
)

// stubHealthyOVN primes every probe with the output of a node where OVN is up.
func stubHealthyOVN(s *stubRunner) {
	s.expect(brExistsCmd, nil, nil)
	s.expect(connStatusCmd, []byte("connected\n"), nil)
	s.expect(systemIDCmd, []byte("\""+healthyChassis+"\"\n"), nil)
	s.expect(encapIPCmd, []byte("\""+healthyEncapIP+"\"\n"), nil)
	s.expect(ovnRemoteCmd, []byte("\""+healthyOVNRemte+"\"\n"), nil)
}

func TestHealthStatus_HealthyNode(t *testing.T) {
	s := newStubRunner()
	stubHealthyOVN(s)

	got := healthStatus(context.Background(), s)

	if !got.BrIntExists {
		t.Error("BrIntExists = false, want true")
	}
	if !got.OVNControllerUp {
		t.Error("OVNControllerUp = false, want true")
	}
	if got.ChassisID != healthyChassis {
		t.Errorf("ChassisID = %q, want %q", got.ChassisID, healthyChassis)
	}
	if got.EncapIP != healthyEncapIP {
		t.Errorf("EncapIP = %q, want %q", got.EncapIP, healthyEncapIP)
	}
	if got.OVNRemote != healthyOVNRemte {
		t.Errorf("OVNRemote = %q, want %q", got.OVNRemote, healthyOVNRemte)
	}
}

// No probe may escalate. The false-negative this replaced came from probing
// ovn-appctl through sudo as spinifex-daemon, which holds no ovn-appctl grant:
// sudo demanded a password, the error was swallowed, and a healthy node reported
// ovn-controller as not_running. The sockets are group-owned instead, so a sudo
// prefix reappearing here would resurrect that bug.
func TestHealthStatus_ProbesDoNotEscalate(t *testing.T) {
	s := newStubRunner()
	stubHealthyOVN(s)

	healthStatus(context.Background(), s)

	if len(s.calls) == 0 {
		t.Fatal("healthStatus issued no probes")
	}
	for _, c := range s.calls {
		if strings.HasPrefix(c, "sudo ") {
			t.Errorf("probe %q escalates; OVS/OVN read probes must run unprivileged", c)
		}
	}
}

// ovn-controller up but not synced with the SB cluster is not readiness: a
// stale-SB wedge must surface as down rather than hide behind process liveness.
func TestHealthStatus_UnsyncedControllerIsDown(t *testing.T) {
	s := newStubRunner()
	stubHealthyOVN(s)
	s.expect(connStatusCmd, []byte("not connected\n"), nil)

	if healthStatus(context.Background(), s).OVNControllerUp {
		t.Error("OVNControllerUp = true for a controller that is not connected to the SB cluster")
	}
}

// A probe that cannot run reports its field as unset, never as healthy.
func TestHealthStatus_FailedProbesReportUnset(t *testing.T) {
	s := newStubRunner()
	s.expect(brExistsCmd, nil, errors.New("no such bridge"))
	s.expect(connStatusCmd, nil, errors.New("no ovn-controller socket"))
	s.expect(systemIDCmd, nil, errors.New("ovsdb unreachable"))
	s.expect(encapIPCmd, nil, errors.New("ovsdb unreachable"))
	s.expect(ovnRemoteCmd, nil, errors.New("ovsdb unreachable"))

	got := healthStatus(context.Background(), s)

	if got != (OVNHealth{}) {
		t.Errorf("healthStatus = %+v on a node where every probe failed, want the zero value", got)
	}
}

// Every tool healthStatus probes must be one the escalation policy classifies as
// unprivileged, or the probe would need a sudoers grant to read a status.
func TestHealthStatus_ProbesAreSocketClients(t *testing.T) {
	s := newStubRunner()
	stubHealthyOVN(s)

	healthStatus(context.Background(), s)

	for _, c := range s.calls {
		tool := strings.Fields(c)[0]
		if utils.NeedsPrivilege(tool) {
			t.Errorf("probe tool %q is classified as needing privilege; health must only use socket clients", tool)
		}
	}
}
