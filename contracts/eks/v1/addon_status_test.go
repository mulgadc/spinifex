package eksv1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAddonStatusSubject(t *testing.T) {
	require.Equal(t, "eks.addon.111122223333.prod.status", AddonStatusSubject("111122223333", "prod"))
}

func TestAddonStatusReportJSONContract(t *testing.T) {
	for _, tc := range []struct {
		report AddonStatusReport
		want   string
	}{
		{
			report: AddonStatusReport{Addon: "coredns", Version: "1.11.3", Phase: AddonPhaseApplied, TS: 1736935200},
			want:   `{"addon":"coredns","version":"1.11.3","phase":"applied","ts":1736935200}`,
		},
		{
			report: AddonStatusReport{Addon: "coredns", Version: "1.11.3", Phase: AddonPhaseReady, TS: 1736935200},
			want:   `{"addon":"coredns","version":"1.11.3","phase":"ready","ts":1736935200}`,
		},
		{
			report: AddonStatusReport{Addon: "coredns", Version: "1.11.3", Phase: AddonPhaseFailed, Message: "rollout stalled", TS: 1736935200},
			want:   `{"addon":"coredns","version":"1.11.3","phase":"failed","message":"rollout stalled","ts":1736935200}`,
		},
	} {
		t.Run(tc.want, func(t *testing.T) {
			data, err := json.Marshal(tc.report)
			require.NoError(t, err)
			require.Equal(t, tc.want, string(data))
		})
	}
}

// mulga-eks-addon-sync.sh builds the report with printf and always sends
// "message", empty when there is nothing to say.
func TestAddonStatusReport_DecodesGuestPrintfForm(t *testing.T) {
	for _, tc := range []struct {
		body string
		want AddonStatusReport
	}{
		{
			body: `{"addon":"aws-load-balancer-controller","version":"2.11.0","phase":"applied","message":"","ts":1736935200}`,
			want: AddonStatusReport{Addon: "aws-load-balancer-controller", Version: "2.11.0", Phase: AddonPhaseApplied, TS: 1736935200},
		},
		{
			body: `{"addon":"aws-load-balancer-controller","version":"2.11.0","phase":"ready","message":"","ts":1736935200}`,
			want: AddonStatusReport{Addon: "aws-load-balancer-controller", Version: "2.11.0", Phase: AddonPhaseReady, TS: 1736935200},
		},
		{
			body: `{"addon":"aws-load-balancer-controller","version":"9.9.9","phase":"failed","message":"no baked bundle for version 9.9.9","ts":1736935200}`,
			want: AddonStatusReport{Addon: "aws-load-balancer-controller", Version: "9.9.9", Phase: AddonPhaseFailed, Message: "no baked bundle for version 9.9.9", TS: 1736935200},
		},
	} {
		t.Run(string(tc.want.Phase), func(t *testing.T) {
			var got AddonStatusReport
			require.NoError(t, json.Unmarshal([]byte(tc.body), &got))
			require.Equal(t, tc.want, got)
		})
	}
}
