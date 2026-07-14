package handlers_quota

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

func vol(id string, size int64) *ec2.Volume {
	return &ec2.Volume{VolumeId: aws.String(id), Size: aws.Int64(size)}
}

// TestSumVolumeGiB covers the size-sum helper: totals are summed, the named
// volume is reported back, and nil/unsized entries contribute nothing. Root/AMI
// volumes never appear in DescribeVolumes, so an empty list models a fresh
// account and confirms the helper charges only data volumes it is given.
func TestSumVolumeGiB(t *testing.T) {
	tests := []struct {
		name       string
		volumes    []*ec2.Volume
		volumeID   string
		wantTotal  int
		wantTarget int
	}{
		{"empty", nil, "", 0, 0},
		{"sum without target", []*ec2.Volume{vol("vol-a", 30), vol("vol-b", 30)}, "", 60, 0},
		{"sum with target", []*ec2.Volume{vol("vol-a", 30), vol("vol-b", 30)}, "vol-a", 60, 30},
		{"nil and unsized skipped", []*ec2.Volume{vol("vol-a", 40), nil, {VolumeId: aws.String("vol-b")}}, "vol-a", 40, 40},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			total, target := sumVolumeGiB(tt.volumes, tt.volumeID)
			if total != tt.wantTotal || target != tt.wantTarget {
				t.Fatalf("sumVolumeGiB() = (%d, %d), want (%d, %d)", total, target, tt.wantTotal, tt.wantTarget)
			}
		})
	}
}

// TestEnforceStorage drives the full per-volume pipeline the wiring methods run
// after DescribeVolumes: sum the live volumes, then compare. CreateVolume charges
// total + requested; ModifyVolume charges total - oldSize + newSize so the
// resized volume's existing size is not double-counted. A snapshot restore is a
// normal sized create, so it is gated identically.
func TestEnforceStorage(t *testing.T) {
	const limit = 100                                             // liveLimits.VolumesGiB
	existing := []*ec2.Volume{vol("vol-a", 30), vol("vol-b", 30)} // 60 GiB in use

	tests := []struct {
		name     string
		op       string // "create" or "modify"
		volumeID string // modify target
		want     int    // create: requested; modify: new size
		wantErr  string
	}{
		{"create under limit", "create", "", 30, ""},
		{"create to limit", "create", "", 40, ""},
		{"create over limit", "create", "", 41, awserrors.ErrorResourceLimitExceeded},
		{"snapshot restore past limit", "create", "", 50, awserrors.ErrorResourceLimitExceeded},
		{"resize to limit", "modify", "vol-a", 70, ""},
		{"resize past limit", "modify", "vol-a", 80, awserrors.ErrorResourceLimitExceeded},
		{"resize shrink never trips", "modify", "vol-a", 1, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			total, target := sumVolumeGiB(existing, tt.volumeID)
			var err error
			switch tt.op {
			case "create":
				err = exceeds(total, tt.want, limit)
			case "modify":
				err = exceeds(total-target, tt.want, limit)
			}
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("%s want nil, got %v (limit %d)", tt.name, err, limit)
			case tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr):
				t.Fatalf("%s = %v, want %q", tt.name, err, tt.wantErr)
			}
		})
	}
}

// Exempt callers must short-circuit before the DescribeVolumes round trip, so the
// gates return nil even with a nil NATS connection (which would otherwise panic
// on describe). Covers both the disabled-config and system-account exemptions for
// create and modify.
func TestEnforceVolumeExemptShortCircuits(t *testing.T) {
	const normalAccount = "123456789012"
	disabled := New(Limits{Enabled: false}, nil)
	enabled := New(liveLimits, nil)
	cases := []struct {
		name string
		fn   func() error
	}{
		{"create disabled", func() error { return disabled.EnforceVolumeCreate(context.Background(), nil, normalAccount, 1) }},
		{"modify disabled", func() error {
			return disabled.EnforceVolumeModify(context.Background(), nil, normalAccount, "vol-a", 1)
		}},
		{"create system account", func() error { return enabled.EnforceVolumeCreate(context.Background(), nil, utils.GlobalAccountID, 1) }},
		{"modify system account", func() error {
			return enabled.EnforceVolumeModify(context.Background(), nil, utils.GlobalAccountID, "vol-a", 1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); err != nil {
				t.Fatalf("exempt path returned %v, want nil", err)
			}
		})
	}
}
