//go:build e2e

package storagefault

import "testing"

const capturedArgv = "nbdkit -f --unix /run/spinifex/nbd/nbd-vol-1.sock plugin.so " +
	"volume=vol-1 base_dir=/var/lib/spinifex/viperblock/ lease_epoch=125 gc_enabled=true"

func TestLeaseEpochArg(t *testing.T) {
	cases := []struct {
		name    string
		cmdline string
		want    uint64
		wantOK  bool
	}{
		{"present", capturedArgv, 125, true},
		{"absent on a format-1 launch", "nbdkit -f --unix /s plugin.so volume=vol-1", 0, false},
		{"not a number", "nbdkit lease_epoch=abc", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := leaseEpochArg(c.cmdline)
			if got != c.want || ok != c.wantOK {
				t.Fatalf("leaseEpochArg = %d, %v; want %d, %v", got, ok, c.want, c.wantOK)
			}
		})
	}
}

func TestWithLeaseEpoch(t *testing.T) {
	want := "nbdkit -f --unix /run/spinifex/nbd/nbd-vol-1.sock plugin.so " +
		"volume=vol-1 base_dir=/var/lib/spinifex/viperblock/ lease_epoch=140 gc_enabled=true"
	if got := withLeaseEpoch(capturedArgv, 140); got != want {
		t.Fatalf("withLeaseEpoch replaced more than the epoch:\n got %s\nwant %s", got, want)
	}
	format1 := "nbdkit -f --unix /s plugin.so volume=vol-1"
	if got := withLeaseEpoch(format1, 140); got != format1 {
		t.Fatalf("withLeaseEpoch added an epoch to an argv without one: %s", got)
	}
}
