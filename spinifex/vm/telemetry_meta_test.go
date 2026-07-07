package vm

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

func TestTelemetryPeriodSeconds(t *testing.T) {
	tests := []struct {
		name string
		vm   *VM
		want int
	}{
		{"no launch input", &VM{}, 300},
		{"monitoring absent", &VM{RunInstancesInput: &ec2.RunInstancesInput{}}, 300},
		{"monitoring disabled", &VM{RunInstancesInput: &ec2.RunInstancesInput{
			Monitoring: &ec2.RunInstancesMonitoringEnabled{Enabled: aws.Bool(false)}}}, 300},
		{"detailed monitoring", &VM{RunInstancesInput: &ec2.RunInstancesInput{
			Monitoring: &ec2.RunInstancesMonitoringEnabled{Enabled: aws.Bool(true)}}}, 60},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := telemetryPeriodSeconds(tt.vm); got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestTelemetryTaps(t *testing.T) {
	v := &VM{
		ID:        "i-0abc",
		ENIId:     "eni-primary",
		ExtraENIs: []ExtraENI{{ENIID: "eni-extra"}},
	}
	v.ENIRequests.AttachedByENIID = map[string]int{"eni-hot": 1, "eni-primary": 0}

	got := telemetryTaps(v)
	want := []string{
		TapDeviceName("eni-extra"),
		TapDeviceName("eni-hot"),
		TapDeviceName("eni-primary"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("taps = %v, want %v (sorted, deduped)", got, want)
	}
}

func TestTelemetryMetaRoundtrip(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	v := &VM{ID: "i-0abc", AccountID: "123456789012"}
	v.Config.CPUCount = 2

	// No telemetry socket configured: no-op, no file.
	if err := writeTelemetryMeta(v); err != nil {
		t.Fatalf("no-op write: %v", err)
	}
	if _, err := os.Stat(utils.TelemetryMetaPath(v.ID)); !os.IsNotExist(err) {
		t.Fatal("no file expected without a telemetry socket")
	}

	v.Config.TelemetryQMPSocket = utils.TelemetryMetaPath(v.ID) + ".sock"
	if err := writeTelemetryMeta(v); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(utils.TelemetryMetaPath(v.ID))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var meta types.GuestTelemetryMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if meta.InstanceID != "i-0abc" || meta.AccountID != "123456789012" ||
		meta.VCPUs != 2 || meta.PeriodSeconds != 300 ||
		meta.Socket != v.Config.TelemetryQMPSocket {
		t.Errorf("meta = %+v", meta)
	}

	removeTelemetryArtifacts(v)
	if _, err := os.Stat(utils.TelemetryMetaPath(v.ID)); !os.IsNotExist(err) {
		t.Error("metadata must be removed")
	}
}
