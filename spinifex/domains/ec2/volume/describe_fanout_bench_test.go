package volume

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
)

// BenchmarkDescribeVolumesByIDs measures one explicit-ID DescribeVolumes
// against a store that takes 2 ms per read, and reports the most reads that
// one request had in flight at once.
func BenchmarkDescribeVolumesByIDs(b *testing.B) {
	for _, n := range []int{10, 100, 1000, 10000} {
		b.Run(fmt.Sprintf("ids=%d", n), func(b *testing.B) {
			defer silenceLogs(b)()
			mem := objectstore.NewMemoryObjectStore()
			ids := make([]string, n)
			meta := ebsmetadata.NewStore(mem, "test-bucket")
			for i := range ids {
				ids[i] = fmt.Sprintf("vol-%017x", n-i)
				if err := meta.PutVolume(context.Background(), ebsmetadata.Volume{
					VolumeID: ids[i], TenantID: testVolAccountID, State: "available", CapacityGiB: 8,
					AvailabilityZone: "ap-southeast-2a", VolumeType: "gp3",
				}); err != nil {
					b.Fatalf("PutVolume: %v", err)
				}
			}
			store := &peakGetStore{ObjectStore: mem, delay: 2 * time.Millisecond}
			cfg := &config.Config{AZ: "ap-southeast-2a", Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
			svc := NewVolumeServiceImplWithStore(cfg, store, nil)
			input := &ec2.DescribeVolumesInput{VolumeIds: aws.StringSlice(ids)}

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				out, err := svc.DescribeVolumes(context.Background(), input, testVolAccountID)
				if err != nil {
					b.Fatalf("DescribeVolumes: %v", err)
				}
				if len(out.Volumes) != n {
					b.Fatalf("got %d volumes, want %d", len(out.Volumes), n)
				}
			}
			b.ReportMetric(float64(store.highest.Load()), "peak-gets")
		})
	}
}
