package ebspolicy

import "testing"

func TestGP3Envelope(t *testing.T) {
	if VolumeTypeGP3 != "gp3" || DefaultGP3IOPS != 3000 || MaxGP3IOPS != 16000 || GP3IOPSPerGiB != 500 || DefaultGP3Throughput != 125 || MaxGP3Throughput != 1000 {
		t.Fatal("unexpected GP3 policy envelope")
	}
}
