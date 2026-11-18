package host

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// EndpointEntry is one gateway-endpoint destination a guest may dial: a node
// address and the gate port behind it.
type EndpointEntry struct {
	Addr netip.Addr
	Port uint16
}

func (e EndpointEntry) String() string { return netip.AddrPortFrom(e.Addr, e.Port).String() }

// endpointCookiePrefix tags gateway-endpoint demux flows on IMDSBridge. It differs
// from imdsCookiePrefix so the IMDS drift count and re-attach never see these
// flows, and endpoint changes never touch IMDS.
const endpointCookiePrefix = "0xa1e5"

// endpointFlowCookie returns the per-tap cookie for endpoint's demux flows.
func endpointFlowCookie(endpoint string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(endpoint))
	return fmt.Sprintf("%s%08x", endpointCookiePrefix, h.Sum32())
}

// normaliseEntries validates entries and returns them sorted and deduplicated.
func normaliseEntries(entries []EndpointEntry) ([]EndpointEntry, error) {
	out := make([]EndpointEntry, 0, len(entries))
	for _, e := range entries {
		if !e.Addr.Is4() || e.Addr.IsUnspecified() {
			return nil, fmt.Errorf("endpoint entry %s: address must be a specified IPv4 address", e)
		}
		if e.Port == 0 {
			return nil, fmt.Errorf("endpoint entry %s: port required", e)
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b EndpointEntry) int {
		if c := a.Addr.Compare(b.Addr); c != 0 {
			return c
		}
		return int(a.Port) - int(b.Port)
	})
	return slices.Compact(out), nil
}

func endpointDemuxMatch(tap string, e EndpointEntry) string {
	return fmt.Sprintf("table=0,priority=%d,in_port=%s,tcp,nw_dst=%s,tp_dst=%d",
		imdsDemuxPriority, tap, e.Addr, e.Port)
}

// EnsureEndpointDemux makes eniID's primary tap send TCP to exactly entries to
// its IMDS endpoint, where the host capture rules hand it to the endpoint proxy.
// Install the capture rules first: a demuxed packet without them times out.
func EnsureEndpointDemux(ctx context.Context, r Runner, eniID string, entries []EndpointEntry) error {
	want, err := normaliseEntries(entries)
	if err != nil {
		return err
	}
	if len(want) == 0 {
		return RemoveEndpointDemux(ctx, r, eniID)
	}
	tap := vm.TapDeviceName(eniID)
	endpoint := IMDSEndpointName(eniID)
	cookie := endpointFlowCookie(endpoint)
	// add-flow over an identical match replaces it in place, so an unchanged
	// entry never loses its flow and established connections are not cut.
	for _, e := range want {
		spec := endpointDemuxMatch(tap, e) + ",actions=mod_dl_dst:" + IMDSEndpointMAC(eniID) + ",output:" + endpoint
		if err := installIMDSFlow(ctx, r, cookie, spec); err != nil {
			return err
		}
	}
	out, err := r.Run(ctx, "ovs-ofctl", "--no-stats", "dump-flows", IMDSBridge, "cookie="+cookie+"/-1")
	if err != nil {
		return fmt.Errorf("dump endpoint demux flows for %s: %w", endpoint, err)
	}
	installed, err := parseEndpointDemuxFlows(out)
	if err != nil {
		return err
	}
	for _, e := range installed {
		if slices.Contains(want, e) {
			continue
		}
		if _, err := r.Run(ctx, "ovs-ofctl", "--strict", "del-flows", IMDSBridge,
			"cookie="+cookie+"/-1,"+endpointDemuxMatch(tap, e)); err != nil {
			return fmt.Errorf("remove endpoint demux flow %s for %s: %w", e, endpoint, err)
		}
	}
	slog.Debug("host: endpoint demux reconciled", "endpoint", endpoint, "entries", len(want))
	return nil
}

// RemoveEndpointDemux removes every endpoint demux flow of eniID's tap and
// leaves its IMDS flows in place. Idempotent.
func RemoveEndpointDemux(ctx context.Context, r Runner, eniID string) error {
	return clearIMDSFlowsByCookie(ctx, r, endpointFlowCookie(IMDSEndpointName(eniID)))
}

// EndpointDemuxStates returns, per primary ENI, the destinations its tap's
// demux flows send to the proxy, and how many demux flows no listed ENI owns,
// from one dump of the bridge. Every ENI asked for is present.
func EndpointDemuxStates(ctx context.Context, r Runner, eniIDs []string) (map[string][]EndpointEntry, int, error) {
	out, err := r.Run(ctx, "ovs-ofctl", "--no-stats", "dump-flows", IMDSBridge,
		"cookie="+endpointCookiePrefix+"00000000/0xffff00000000")
	if err != nil {
		return nil, 0, fmt.Errorf("dump endpoint demux flows: %w", err)
	}
	byCookie, err := endpointDemuxByCookie(out)
	if err != nil {
		return nil, 0, err
	}
	states := make(map[string][]EndpointEntry, len(eniIDs))
	for _, eniID := range eniIDs {
		cookie, err := strconv.ParseUint(endpointFlowCookie(IMDSEndpointName(eniID)), 0, 64)
		if err != nil {
			return nil, 0, fmt.Errorf("parse endpoint cookie for %s: %w", eniID, err)
		}
		states[eniID] = byCookie[cookie]
		delete(byCookie, cookie)
	}
	unowned := 0
	for _, es := range byCookie {
		unowned += len(es)
	}
	return states, unowned, nil
}

// parseEndpointDemuxFlows reads the destinations of dumped demux flows.
func parseEndpointDemuxFlows(out []byte) ([]EndpointEntry, error) {
	byCookie, err := endpointDemuxByCookie(out)
	if err != nil {
		return nil, err
	}
	var entries []EndpointEntry
	for _, es := range byCookie {
		entries = append(entries, es...)
	}
	return entries, nil
}

// endpointDemuxByCookie reads dumped demux flows' destinations, grouped by the
// cookie each flow carries.
func endpointDemuxByCookie(out []byte) (map[uint64][]EndpointEntry, error) {
	byCookie := map[uint64][]EndpointEntry{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "cookie=") {
			continue
		}
		match, _, _ := strings.Cut(line, " actions=")
		var cookie, addr, port string
		for f := range strings.SplitSeq(strings.ReplaceAll(match, ", ", ","), ",") {
			if v, ok := strings.CutPrefix(f, "cookie="); ok {
				cookie = v
			} else if v, ok := strings.CutPrefix(f, "nw_dst="); ok {
				addr = v
			} else if v, ok := strings.CutPrefix(f, "tp_dst="); ok {
				port = v
			}
		}
		c, err := strconv.ParseUint(cookie, 0, 64)
		if err != nil {
			return nil, fmt.Errorf("parse endpoint demux flow %q: %w", line, err)
		}
		a, err := netip.ParseAddr(addr)
		if err != nil {
			return nil, fmt.Errorf("parse endpoint demux flow %q: %w", line, err)
		}
		p, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("parse endpoint demux flow %q: %w", line, err)
		}
		byCookie[c] = append(byCookie[c], EndpointEntry{Addr: a, Port: uint16(p)})
	}
	return byCookie, sc.Err()
}
