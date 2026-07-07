package vm

import (
	"encoding/json"
	"log/slog"
	"os"
	"sort"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// telemetryPeriodSeconds maps the instance's EC2 monitoring tier to the
// collection interval: 60s detailed when requested at launch, else 300s basic.
func telemetryPeriodSeconds(v *VM) int {
	if v.RunInstancesInput != nil && v.RunInstancesInput.Monitoring != nil &&
		aws.BoolValue(v.RunInstancesInput.Monitoring.Enabled) {
		return 60
	}
	return 300
}

// telemetryTaps lists the VPC data-plane taps whose host counters yield
// NetworkIn/Out: primary ENI, launch-time extra ENIs, and hot-plugged ENIs.
// Callers must not hold v.ENIRequests.Mu.
func telemetryTaps(v *VM) []string {
	seen := map[string]bool{}
	var taps []string
	add := func(eniID string) {
		if eniID == "" {
			return
		}
		tap := TapDeviceName(eniID)
		if !seen[tap] {
			seen[tap] = true
			taps = append(taps, tap)
		}
	}
	add(v.ENIId)
	for _, e := range v.ExtraENIs {
		add(e.ENIID)
	}
	v.ENIRequests.Mu.Lock()
	for eniID := range v.ENIRequests.AttachedByENIID {
		add(eniID)
	}
	v.ENIRequests.Mu.Unlock()
	sort.Strings(taps)
	return taps
}

// writeTelemetryMeta (re)writes the qmp-collector discovery file for a VM with
// a telemetry QMP socket. Telemetry must never block a launch, so callers only
// log failures. No-op when the telemetry socket was not configured.
func writeTelemetryMeta(v *VM) error {
	if v.Config.TelemetryQMPSocket == "" {
		return nil
	}
	meta := types.GuestTelemetryMeta{
		InstanceID:    v.ID,
		AccountID:     v.AccountID,
		VCPUs:         v.Config.CPUCount,
		PeriodSeconds: telemetryPeriodSeconds(v),
		Taps:          telemetryTaps(v),
		Socket:        v.Config.TelemetryQMPSocket,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(utils.TelemetryMetaPath(v.ID), data, 0o600)
}

// refreshTelemetryMeta rewrites the discovery file after an ENI change; safe
// to call on any path (idempotent, tolerates missing telemetry socket).
func refreshTelemetryMeta(v *VM) {
	if err := writeTelemetryMeta(v); err != nil {
		slog.Warn("Failed to refresh telemetry metadata", "instanceId", v.ID, "err", err)
	}
}

// removeTelemetryArtifacts deletes the telemetry socket and discovery file so
// the collector stops polling a gone VM.
func removeTelemetryArtifacts(v *VM) {
	if v.Config.TelemetryQMPSocket != "" {
		_ = os.Remove(v.Config.TelemetryQMPSocket)
	}
	_ = os.Remove(utils.TelemetryMetaPath(v.ID))
}
