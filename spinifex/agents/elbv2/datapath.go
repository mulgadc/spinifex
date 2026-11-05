package elbv2

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// netcfgPath is the fw_cfg blob the microVM init script reads to configure NICs.
	netcfgPath = "/sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/netcfg/raw"

	sysClassNet = "/sys/class/net"

	// probeFailureThreshold is the number of consecutive missed probes after
	// which a NIC is reported as failing (about 15 s at the poll interval).
	probeFailureThreshold = 3

	// arpExchangeWait bounds one ARP exchange. probeWait bounds how long a tick
	// waits for all probes, so a stuck probe never delays the heartbeat.
	arpExchangeWait = time.Second
	probeWait       = 1500 * time.Millisecond
)

// nicConfig is one NIC of the netcfg blob that has a gateway to probe.
type nicConfig struct {
	MAC     string
	Addr    netip.Addr
	Gateway netip.Addr
}

// NICReport is the per-NIC data-path evidence carried in the heartbeat.
type NICReport struct {
	MAC                 string
	ProbeOK             bool
	ConsecutiveFailures int64
	RxPackets           int64
	TxPackets           int64
	RxDropped           int64
	TxDropped           int64
}

// arpProbeFn sends one ARP request for gateway out of iface and returns nil
// when the gateway's reply arrives within timeout.
type arpProbeFn func(iface string, mac net.HardwareAddr, src, gateway netip.Addr, timeout time.Duration) error

// parseNetcfg reads the KEY=value netcfg blob and returns the NICs that have a
// gateway, in NIC order. The mgmt NIC carries no gateway and is skipped, as
// is any NIC without a usable MAC, address or gateway.
func parseNetcfg(blob []byte) []nicConfig {
	type raw struct{ mac, cidr, gw string }
	byIndex := map[int]*raw{}

	sc := bufio.NewScanner(bytes.NewReader(blob))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, val, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(key, "NIC") {
			continue
		}
		idxStr, field, ok := strings.Cut(strings.TrimPrefix(key, "NIC"), "_")
		if !ok {
			continue
		}
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			continue
		}
		r := byIndex[idx]
		if r == nil {
			r = &raw{}
			byIndex[idx] = r
		}
		switch field {
		case "MAC":
			r.mac = strings.ToLower(val)
		case "CIDR":
			r.cidr = val
		case "GW":
			r.gw = val
		}
	}

	idxs := make([]int, 0, len(byIndex))
	for i := range byIndex {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)

	var out []nicConfig
	for _, i := range idxs {
		r := byIndex[i]
		if r.mac == "" || r.cidr == "" || r.gw == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(r.cidr)
		if err != nil || !prefix.Addr().Is4() {
			continue
		}
		gw, err := netip.ParseAddr(r.gw)
		if err != nil || !gw.Is4() {
			continue
		}
		out = append(out, nicConfig{MAC: r.mac, Addr: prefix.Addr(), Gateway: gw})
	}
	return out
}

// ifaceByMAC returns the interface under sysNetDir whose address is mac.
func ifaceByMAC(sysNetDir, mac string) (string, error) {
	entries, err := os.ReadDir(sysNetDir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(sysNetDir, e.Name(), "address"))
		if err != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(string(b)), mac) {
			return e.Name(), nil
		}
	}
	return "", fmt.Errorf("no interface with MAC %s", mac)
}

// readCounters fills the sysfs statistics of iface into r.
func readCounters(sysNetDir, iface string, r *NICReport) {
	read := func(name string) int64 {
		b, err := os.ReadFile(filepath.Join(sysNetDir, iface, "statistics", name))
		if err != nil {
			return 0
		}
		v, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		return v
	}
	r.RxPackets = read("rx_packets")
	r.TxPackets = read("tx_packets")
	r.RxDropped = read("rx_dropped")
	r.TxDropped = read("tx_dropped")
}

var errARPNoReply = errors.New("no ARP reply from the gateway")

// buildARPRequest returns the Ethernet frame asking who has target.
func buildARPRequest(mac net.HardwareAddr, src, target netip.Addr) []byte {
	f := make([]byte, 0, 42)
	f = append(f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
	f = append(f, mac...)
	f = binary.BigEndian.AppendUint16(f, 0x0806)
	f = binary.BigEndian.AppendUint16(f, 1)      // Ethernet
	f = binary.BigEndian.AppendUint16(f, 0x0800) // IPv4
	f = append(f, 6, 4)
	f = binary.BigEndian.AppendUint16(f, 1) // request
	f = append(f, mac...)
	s := src.As4()
	f = append(f, s[:]...)
	f = append(f, 0, 0, 0, 0, 0, 0)
	t := target.As4()
	f = append(f, t[:]...)
	return f
}

// isARPReplyFrom reports whether frame is an ARP reply sent by sender to mac.
func isARPReplyFrom(frame []byte, mac net.HardwareAddr, sender netip.Addr) bool {
	if len(frame) < 42 || binary.BigEndian.Uint16(frame[12:14]) != 0x0806 {
		return false
	}
	if binary.BigEndian.Uint16(frame[20:22]) != 2 || !bytes.Equal(frame[32:38], mac) {
		return false
	}
	s := sender.As4()
	return bytes.Equal(frame[28:32], s[:])
}

// nicProbeState carries the miss counter of one NIC across ticks. A probe that
// has not returned is still in flight, and counts as a miss without starting
// another goroutine behind it.
type nicProbeState struct {
	mu       sync.Mutex
	failures int64
	inflight bool
}

type probeResult struct {
	ok  bool
	set bool
}

// probeNICs probes every gateway-bearing data NIC once, in parallel, and
// returns within probeWait however long a probe blocks.
func (a *Agent) probeNICs() []NICReport {
	blob, err := os.ReadFile(a.netcfgPath)
	if err != nil {
		return nil
	}
	nics := parseNetcfg(blob)
	if len(nics) == 0 {
		return nil
	}

	reports := make([]NICReport, len(nics))
	results := make([]chan probeResult, len(nics))
	for i, n := range nics {
		results[i] = make(chan probeResult, 1)
		st := a.probeState(n.MAC)

		st.mu.Lock()
		busy := st.inflight
		if !busy {
			st.inflight = true
		}
		st.mu.Unlock()
		if busy {
			results[i] <- probeResult{set: true}
			continue
		}

		go func(n nicConfig, st *nicProbeState, out chan<- probeResult) {
			ok := a.probeOne(n)
			st.mu.Lock()
			st.inflight = false
			st.mu.Unlock()
			out <- probeResult{ok: ok, set: true}
		}(n, st, results[i])
	}

	deadline := time.NewTimer(a.probeWait)
	defer deadline.Stop()
	expired := false
	for i, n := range nics {
		var res probeResult
		if expired {
			select {
			case res = <-results[i]:
			default:
			}
		} else {
			select {
			case res = <-results[i]:
			case <-deadline.C:
				// Out of time: an unfinished probe is a miss for this tick.
				expired = true
			}
		}

		st := a.probeState(n.MAC)
		st.mu.Lock()
		if res.ok {
			st.failures = 0
		} else {
			st.failures++
		}
		failures := st.failures
		st.mu.Unlock()

		r := NICReport{
			MAC:                 n.MAC,
			ProbeOK:             failures < probeFailureThreshold,
			ConsecutiveFailures: failures,
		}
		if iface, err := ifaceByMAC(a.sysNetDir, n.MAC); err == nil {
			readCounters(a.sysNetDir, iface, &r)
		}
		reports[i] = r
	}
	return reports
}

// probeOne runs one ARP exchange for n, false on any failure.
func (a *Agent) probeOne(n nicConfig) bool {
	iface, err := ifaceByMAC(a.sysNetDir, n.MAC)
	if err != nil {
		return false
	}
	hw, err := net.ParseMAC(n.MAC)
	if err != nil {
		return false
	}
	return a.arpProbeFn(iface, hw, n.Addr, n.Gateway, arpExchangeWait) == nil
}

func (a *Agent) probeState(mac string) *nicProbeState {
	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	st := a.probeStates[mac]
	if st == nil {
		st = &nicProbeState{}
		a.probeStates[mac] = st
	}
	return st
}
