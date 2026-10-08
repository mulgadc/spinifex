package eksv1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// A v1 decoder ignores fields a later sender adds, so a report carrying them
// decodes to the same value as one without.
func TestAddonStatusReport_IgnoresUnknownFields(t *testing.T) {
	body := `{"addon":"argocd","version":"3.0.23","phase":"ready","message":"","ts":1736935200,` +
		`"incarnationId":"inc-1","generation":4,"executorEpoch":7}`
	var got AddonStatusReport
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.Equal(t, AddonStatusReport{Addon: "argocd", Version: "3.0.23", Phase: AddonPhaseReady, TS: 1736935200}, got)
}

// Both deployed body forms tolerate added top-level and per-add-on fields.
func TestInternalAddonsResponse_IgnoresUnknownFields(t *testing.T) {
	for name, body := range map[string]string{
		"rest-json": `{"Addons":[{"AddonName":"spinifex-noop","AddonVersion":"0.1.0","ServiceAccountRoleArn":"",` +
			`"ConfigurationValues":"","IncarnationId":"inc-1","Generation":4}],"ExecutorEpoch":7}`,
		"encoding-json": `{"addons":[{"addonName":"spinifex-noop","addonVersion":"0.1.0",` +
			`"incarnationId":"inc-1","generation":4}],"executorEpoch":7}`,
	} {
		t.Run(name, func(t *testing.T) {
			var got InternalAddonsResponse
			require.NoError(t, json.Unmarshal([]byte(body), &got))
			require.Equal(t, InternalAddonsResponse{Addons: []StagedAddonManifest{{AddonName: "spinifex-noop", AddonVersion: "0.1.0"}}}, got)
		})
	}
}
