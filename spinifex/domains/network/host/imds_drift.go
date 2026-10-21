package host

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// IMDSTapFlowCount is how many flows installIMDSDatapath installs under a tap's
// cookie: an ARP responder and an ingress demux per captured address, the
// egress flow, and the two tap<->patch forward flows.
const IMDSTapFlowCount = 7

// IMDSFlowCookie is the cookie every br-imds flow of eniID's datapath carries.
func IMDSFlowCookie(eniID string) string {
	return imdsFlowCookie(IMDSEndpointName(eniID))
}

// endpointSysctlDir is where the kernel exposes per-device IPv4 settings.
var endpointSysctlDir = "/proc/sys/net/ipv4/conf"

// IMDSDatapathStates reports what survives of each primary ENI's datapath. An
// ovs-vswitchd restart drops every flow. A kernel-module reload re-creates the
// endpoint, which resets its sysctls and drops its reply rule and route.
func IMDSDatapathStates(ctx context.Context, r Runner, eniIDs []string) (map[string]vm.IMDSDatapathState, error) {
	flows, err := r.Run(ctx, "ovs-ofctl", "--no-stats", "dump-flows", IMDSBridge)
	if err != nil {
		return nil, fmt.Errorf("dump %s flows: %w", IMDSBridge, err)
	}
	byCookie, err := countFlowsByCookie(flows)
	if err != nil {
		return nil, err
	}
	states := make(map[string]vm.IMDSDatapathState, len(eniIDs))
	for _, eniID := range eniIDs {
		endpoint := IMDSEndpointName(eniID)
		cookie, err := strconv.ParseUint(imdsFlowCookie(endpoint), 0, 64)
		if err != nil {
			return nil, fmt.Errorf("parse IMDS cookie for %s: %w", eniID, err)
		}
		states[eniID] = vm.IMDSDatapathState{
			FlowsFound:    byCookie[cookie],
			FlowsExpected: IMDSTapFlowCount,
			EndpointReady: endpointConfigured(endpoint),
		}
	}
	return states, nil
}

// countFlowsByCookie tallies `ovs-ofctl dump-flows` lines by cookie. A flow
// with a zero cookie is printed without the field and is not counted.
func countFlowsByCookie(out []byte) (map[uint64]int, error) {
	counts := map[uint64]int{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		rest, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "cookie=")
		if !ok {
			continue
		}
		val, _, _ := strings.Cut(rest, ",")
		cookie, err := strconv.ParseUint(val, 0, 64)
		if err != nil {
			return nil, fmt.Errorf("parse flow cookie %q: %w", val, err)
		}
		counts[cookie]++
	}
	return counts, sc.Err()
}

// endpointConfigured reports whether endpoint carries the accept_local the
// install sets. Read from /proc rather than through ip, which the daemon can
// only run under sudo; the device's reply rule and route go with the setting.
func endpointConfigured(endpoint string) bool {
	val, err := os.ReadFile(filepath.Join(endpointSysctlDir, endpoint, "accept_local"))
	return err == nil && strings.TrimSpace(string(val)) == "1"
}

// repairIMDSDatapath re-installs a tap's datapath after part of it was lost.
// It refuses a tap that is no longer on br-imds: installing would re-create the
// patch and endpoint of a guest being torn down, which vpcd and OVN then claim.
func repairIMDSDatapath(ctx context.Context, r Runner, d IMDSTapDatapath) error {
	out, err := r.Run(ctx, "ovs-vsctl", "list-ports", IMDSBridge)
	if err != nil {
		return fmt.Errorf("list %s ports: %w", IMDSBridge, err)
	}
	if !slices.Contains(strings.Fields(string(out)), d.Tap) {
		return vm.ErrIMDSTapDetached
	}
	return installIMDSDatapath(ctx, r, d)
}
