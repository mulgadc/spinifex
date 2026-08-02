package host

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/vm"
)

// listInterfacesCmd is the single query ListIMDSTaps issues per pass.
const listInterfacesCmd = "ovs-vsctl --format=json --columns=name,external_ids list Interface"

// ovsListJSON renders the `ovs-vsctl --format=json list` envelope for a set of
// interfaces, so the tests exercise the real decode path rather than a hand-typed
// blob. A nil external_ids renders as OVSDB's empty map.
func ovsListJSON(t *testing.T, ifaces []struct {
	name string
	ids  map[string]string
},
) []byte {
	t.Helper()
	data := make([]any, 0, len(ifaces))
	for _, iface := range ifaces {
		pairs := make([][]string, 0, len(iface.ids))
		for k, v := range iface.ids {
			pairs = append(pairs, []string{k, v})
		}
		data = append(data, []any{iface.name, []any{"map", pairs}})
	}
	out, err := json.Marshal(map[string]any{
		"headings": []string{"name", "external_ids"},
		"data":     data,
	})
	if err != nil {
		t.Fatalf("marshal ovs-vsctl JSON: %v", err)
	}
	return out
}

// ListIMDSTaps must enumerate only the IMDS patch ports (imi-*), recover the
// *full* ENI from each port's iface-id ("port-<eniID>"), and pair it with the
// br-imds endpoint name — ignoring guest taps, the br-imds-end patch and the
// endpoint port, all of which are in the same unfiltered interface list.
func TestListIMDSTaps_RecoversENIFromPatchIfaceID(t *testing.T) {
	const (
		eniA = "eni-0aaa1111deadbeef"
		eniB = "eni-0bbb2222cafef00d"
	)
	s := newStubRunner()
	s.expect(listInterfacesCmd, ovsListJSON(t, []struct {
		name string
		ids  map[string]string
	}{
		{name: IMDSIntPatchPort(eniA), ids: map[string]string{"iface-id": vm.OVSIfaceID(eniA), "attached-mac": "52:54:00:aa:bb:cc"}},
		{name: "tap0deadbeef"},
		{name: IMDSIntPatchPort(eniB), ids: map[string]string{"iface-id": vm.OVSIfaceID(eniB)}},
		// The br-imds patch end and the endpoint carry neighbouring prefixes; both
		// must fall outside the imi- filter.
		{name: IMDSPatchPort(eniA)},
		{name: IMDSEndpointName(eniA)},
	}), nil)

	taps, err := ListIMDSTaps(context.Background(), s)
	if err != nil {
		t.Fatalf("ListIMDSTaps: %v", err)
	}

	got := map[string]string{}
	for _, tp := range taps {
		got[tp.ENIID] = tp.Endpoint
	}
	want := map[string]string{
		eniA: IMDSEndpointName(eniA),
		eniB: IMDSEndpointName(eniB),
	}
	if len(got) != len(want) {
		t.Fatalf("ListIMDSTaps returned %d taps, want %d: %v", len(got), len(want), taps)
	}
	for eni, ep := range want {
		if got[eni] != ep {
			t.Errorf("endpoint for %s = %q, want %q", eni, got[eni], ep)
		}
	}
}

// The pass costs exactly one OVS invocation however many taps are live. It runs
// on vpcd's 15s tick, and the per-port query it replaced was 1+N sudo calls a
// tick — the bulk of the service journal.
func TestListIMDSTaps_IssuesOneQueryPerPass(t *testing.T) {
	ifaces := make([]struct {
		name string
		ids  map[string]string
	}, 0, 4)
	for _, eni := range []string{"eni-01", "eni-02", "eni-03", "eni-04"} {
		ifaces = append(ifaces, struct {
			name string
			ids  map[string]string
		}{name: IMDSIntPatchPort(eni), ids: map[string]string{"iface-id": vm.OVSIfaceID(eni)}})
	}
	s := newStubRunner()
	s.expect(listInterfacesCmd, ovsListJSON(t, ifaces), nil)

	taps, err := ListIMDSTaps(context.Background(), s)
	if err != nil {
		t.Fatalf("ListIMDSTaps: %v", err)
	}
	if len(taps) != 4 {
		t.Fatalf("ListIMDSTaps returned %d taps, want 4", len(taps))
	}
	if n := len(s.calls); n != 1 {
		t.Fatalf("ListIMDSTaps issued %d commands, want exactly 1: %v", n, s.calls)
	}
}

// A patch port whose iface-id is unexpected — malformed (missing the "port-"
// prefix) or absent entirely — is skipped, not fatal. The iface-id is the only
// record of the full ENI, so such a port cannot join the live set either way, and
// aborting would stall every pass for the healthy ports behind it.
func TestListIMDSTaps_SkipsUnexpectedIfaceID(t *testing.T) {
	const (
		eniOK      = "eni-0aaa1111deadbeef"
		eniWrong   = "eni-0ccc3333beeff00d"
		eniMissing = "eni-0ddd4444f00dcafe"
	)
	s := newStubRunner()
	s.expect(listInterfacesCmd, ovsListJSON(t, []struct {
		name string
		ids  map[string]string
	}{
		{name: IMDSIntPatchPort(eniOK), ids: map[string]string{"iface-id": vm.OVSIfaceID(eniOK)}},
		{name: IMDSIntPatchPort(eniWrong), ids: map[string]string{"iface-id": "bogus"}},
		{name: IMDSIntPatchPort(eniMissing), ids: map[string]string{"attached-mac": "52:54:00:aa:bb:cc"}},
	}), nil)

	taps, err := ListIMDSTaps(context.Background(), s)
	if err != nil {
		t.Fatalf("ListIMDSTaps: %v", err)
	}
	if len(taps) != 1 || taps[0].ENIID != eniOK {
		t.Fatalf("ListIMDSTaps = %v, want only %s", taps, eniOK)
	}
}

// TestOVSIfaceIDPrefixMatchesVM keeps the "port-" prefix ListIMDSTaps strips to
// recover the full ENI in sync with the prefix vm.OVSIfaceID / topology.Port
// prepend. host inlines the constant (it cannot import the value without an
// import cycle); if the prefix drifted, ListIMDSTaps would recover no ENIs and
// every IMDS responder would silently stop.
func TestOVSIfaceIDPrefixMatchesVM(t *testing.T) {
	const sentinel = "SENTINEL"
	got := strings.TrimSuffix(vm.OVSIfaceID(sentinel), sentinel)
	if got != ovsIfaceIDPrefix {
		t.Fatalf("ovsIfaceIDPrefix (%q) != vm.OVSIfaceID prefix (%q): ListIMDSTaps would recover no ENIs and all IMDS responders would stop", ovsIfaceIDPrefix, got)
	}
}

// A failed query is fatal to one reconcile pass (the caller retries on the next
// tick), never silently empty. Returning no taps would make reconcile treat every
// guest's healthy responder as stale and stop it on a transient OVS error.
func TestListIMDSTaps_QueryErrorPropagates(t *testing.T) {
	s := newStubRunner()
	s.expect(listInterfacesCmd, nil, errors.New("ovs down"))
	if _, err := ListIMDSTaps(context.Background(), s); err == nil {
		t.Fatal("expected error when the interface query fails; a silent empty result would stop every healthy responder")
	}
}

// Unparseable output is an error too, for the same reason: it must not decode to
// an empty live set.
func TestListIMDSTaps_MalformedJSONPropagates(t *testing.T) {
	s := newStubRunner()
	s.expect(listInterfacesCmd, []byte("not json"), nil)
	if _, err := ListIMDSTaps(context.Background(), s); err == nil {
		t.Fatal("expected error on unparseable ovs-vsctl output")
	}
}

// Column positions are read from the headings, not assumed to follow the
// --columns order, so a reordering by ovs-vsctl cannot silently swap the fields.
func TestParseOVSInterfaceRows_HonoursHeadingOrder(t *testing.T) {
	out := []byte(`{"headings":["external_ids","name"],"data":[[["map",[["iface-id","port-eni-1"]]],"imi-abc"]]}`)
	rows, err := parseOVSInterfaceRows(out)
	if err != nil {
		t.Fatalf("parseOVSInterfaceRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].name != "imi-abc" {
		t.Errorf("name = %q, want imi-abc", rows[0].name)
	}
	if rows[0].externalIDs["iface-id"] != "port-eni-1" {
		t.Errorf("iface-id = %q, want port-eni-1", rows[0].externalIDs["iface-id"])
	}
}
