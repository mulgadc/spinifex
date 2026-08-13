package handlers_ec2_volume

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
)

// silenceLogs discards slog output for the duration of a benchmark's timed
// loop, so per-call INFO logging (which scales with iteration count) doesn't
// dominate the measured cost. Restores the previous default logger on return.
func silenceLogs(b *testing.B) func() {
	b.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	return func() { slog.SetDefault(prev) }
}

// seedProviderVolumes creates n volumes under accountID and returns the ID of
// the last one created, so callers can benchmark a single-ID lookup against a
// bucket holding n volume documents.
func seedProviderVolumes(b *testing.B, svc *VolumeServiceImpl, n int, accountID string) string {
	b.Helper()
	ctx := context.Background()
	var lastID string
	for i := range n {
		vol, err := svc.CreateVolume(ctx, &ec2.CreateVolumeInput{
			Size: aws.Int64(8), AvailabilityZone: aws.String("ap-southeast-2a"),
		}, accountID)
		if err != nil {
			b.Fatalf("seed volume %d: %v", i, err)
		}
		lastID = aws.StringValue(vol.VolumeId)
	}
	return lastID
}

// BenchmarkDescribeVolumes_Provider_SingleID measures the direct-GetVolume
// fast path as the bucket grows. Before this change, a single-ID request
// went through ListVolumes and filtered in memory, so cost scaled with the
// number of volumes in the bucket (N document reads for 1 requested). The
// fast path should stay flat across bucket sizes.
func BenchmarkDescribeVolumes_Provider_SingleID(b *testing.B) {
	for _, n := range []int{1, 10, 100, 500} {
		b.Run(fmt.Sprintf("volumes=%d", n), func(b *testing.B) {
			svc := newTestVolumeService("ap-southeast-2a")
			svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
			targetID := seedProviderVolumes(b, svc, n, "acct-bench")
			ctx := context.Background()
			input := &ec2.DescribeVolumesInput{VolumeIds: []*string{aws.String(targetID)}}

			defer silenceLogs(b)()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := svc.DescribeVolumes(ctx, input, "acct-bench"); err != nil {
					b.Fatalf("DescribeVolumes: %v", err)
				}
			}
		})
	}
}

// BenchmarkDescribeVolumes_Provider_ListAndFilter measures the pre-fix
// approach directly (ListVolumes + in-memory filter for one ID), reusing the
// same seeded buckets as BenchmarkDescribeVolumes_Provider_SingleID so the
// two can be compared at matching bucket sizes to show how listing scales
// with N while the fast path (above) does not.
func BenchmarkDescribeVolumes_Provider_ListAndFilter(b *testing.B) {
	for _, n := range []int{1, 10, 100, 500} {
		b.Run(fmt.Sprintf("volumes=%d", n), func(b *testing.B) {
			svc := newTestVolumeService("ap-southeast-2a")
			svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
			targetID := seedProviderVolumes(b, svc, n, "acct-bench")
			ctx := context.Background()

			defer silenceLogs(b)()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				metadata, err := svc.metadata.ListVolumes(ctx)
				if err != nil {
					b.Fatalf("ListVolumes: %v", err)
				}
				var found *ec2.Volume
				for _, meta := range metadata {
					if meta.TenantID != "acct-bench" || meta.VolumeID != targetID {
						continue
					}
					found = metadataVolumeToEC2(meta)
				}
				if found == nil {
					b.Fatalf("volume %s not found", targetID)
				}
			}
		})
	}
}
