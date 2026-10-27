package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	route53domain "github.com/mulgadc/spinifex/spinifex/domains/route53"
)

type adminCall struct {
	subject string
	body    string
}

// fakeRoute53Admin records each request and answers with reply.
func fakeRoute53Admin(t *testing.T, reply any) *[]adminCall {
	t.Helper()
	var calls []adminCall
	prev := route53AdminFn
	route53AdminFn = func(_ context.Context, subject string, in any) (json.RawMessage, error) {
		body, err := json.Marshal(in)
		require.NoError(t, err)
		calls = append(calls, adminCall{subject, string(body)})
		return json.Marshal(reply)
	}
	t.Cleanup(func() { route53AdminFn = prev })
	return &calls
}

func runCmd(t *testing.T, cmd *cobra.Command, flags map[string]string, args ...string) (string, error) {
	t.Helper()
	for k, v := range flags {
		require.NoError(t, cmd.Flags().Set(k, v))
		t.Cleanup(func() { _ = cmd.Flags().Set(k, cmd.Flags().Lookup(k).DefValue) })
	}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(t.Context())
	err := cmd.RunE(cmd, args)
	return out.String(), err
}

var sampleZone = route53domain.ZoneRecord{
	ID: "Z123", AccountID: "000000000011", Name: "example.com.", State: route53domain.StatePendingNameservers,
	StateReason: "the com name servers delegates example.com to name servers that are not this cluster's: dana.ns.cloudflare.com",
	CheckedAt:   time.Date(2026, 10, 24, 1, 2, 3, 0, time.UTC), DelegationSource: "the com name servers",
	Delegation: []string{"dana.ns.cloudflare.com."},
}

func TestZoneStatusQueriesByIDOrName(t *testing.T) {
	calls := fakeRoute53Admin(t, route53domain.AdminZones{Zones: []route53domain.ZoneRecord{sampleZone}})
	out, err := runCmd(t, dnsZoneStatusCmd, nil, "example.com")
	require.NoError(t, err)
	_, err = runCmd(t, dnsZoneStatusCmd, nil, "/hostedzone/Z123")
	require.NoError(t, err)

	assert.Equal(t, []adminCall{
		{"route53.admin.FindZones", `{"name":"example.com"}`},
		{"route53.admin.FindZones", `{"zone_id":"/hostedzone/Z123"}`},
	}, *calls)
	for _, want := range []string{"Z123", "000000000011", "example.com", "PENDING_NAMESERVERS", "2026-10-24T01:02:03Z", "dana.ns.cloudflare.com.", "not this cluster's"} {
		assert.Contains(t, out, want)
	}
}

func TestZoneStatusReportsNoMatch(t *testing.T) {
	fakeRoute53Admin(t, route53domain.AdminZones{})
	_, err := runCmd(t, dnsZoneStatusCmd, nil, "nothing.example")
	require.ErrorContains(t, err, "no hosted zone matches")
}

func TestZoneOverridesNeedAReason(t *testing.T) {
	calls := fakeRoute53Admin(t, route53domain.AdminZones{Zones: []route53domain.ZoneRecord{sampleZone}})
	_, err := runCmd(t, dnsZoneActivateCmd, nil, "Z123")
	require.ErrorContains(t, err, "--reason is required")
	assert.Empty(t, *calls, "a refused override sends nothing")

	_, err = runCmd(t, dnsZoneDeactivateCmd, map[string]string{"reason": "squatting"}, "Z123")
	require.NoError(t, err)
	_, err = runCmd(t, dnsZoneReleaseCmd, nil, "Z123")
	require.NoError(t, err)
	assert.Equal(t, []adminCall{
		{"route53.admin.Override", `{"zone_id":"Z123","override":"SUSPENDED","reason":"squatting"}`},
		{"route53.admin.Override", `{"zone_id":"Z123","override":"","reason":""}`},
	}, *calls)
}

func TestDelegateReadsNamesFromArgsAndFile(t *testing.T) {
	calls := fakeRoute53Admin(t, route53domain.AdminDelegateOutput{Applied: 3})
	path := filepath.Join(t.TempDir(), "names")
	require.NoError(t, os.WriteFile(path, []byte("# e2e\nb.r53e2e.spx3.net\n\n c.r53e2e.spx3.net \n"), 0o600))

	out, err := runCmd(t, dnsDelegateCmd, map[string]string{"file": path, "remove": "true"}, "a.r53e2e.spx3.net")
	require.NoError(t, err)
	assert.Equal(t, []adminCall{{"route53.admin.Delegate", `{"names":["a.r53e2e.spx3.net","b.r53e2e.spx3.net","c.r53e2e.spx3.net"],"remove":true}`}}, *calls)
	assert.Contains(t, out, "Withdrew the delegation of 3 name(s); 3 record change(s) applied")
}

func TestDelegateNeedsNames(t *testing.T) {
	calls := fakeRoute53Admin(t, route53domain.AdminDelegateOutput{})
	_, err := runCmd(t, dnsDelegateCmd, nil)
	require.ErrorContains(t, err, "no names given")
	assert.Empty(t, *calls)
}
