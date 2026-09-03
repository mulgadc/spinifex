package handlers_ec2_snapshot_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	handlers_ec2_snapshot "github.com/mulgadc/spinifex/spinifex/handlers/ec2/snapshot"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fanoutAccountID = "111122223333"

// slowMetadataStore delays every snapshot metadata read and records how many
// were in flight at once, standing in for an object store under load.
type slowMetadataStore struct {
	objectstore.ObjectStore

	delay time.Duration

	mu      sync.Mutex
	inFlite int
	peak    int
}

func (s *slowMetadataStore) GetObject(ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	s.mu.Lock()
	s.inFlite++
	if s.inFlite > s.peak {
		s.peak = s.inFlite
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlite--
		s.mu.Unlock()
	}()

	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.ObjectStore.GetObject(ctx, input)
}

func seedSnapshots(t *testing.T, store objectstore.ObjectStore, n int) *handlers_ec2_snapshot.SnapshotServiceImpl {
	t.Helper()
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	svc := handlers_ec2_snapshot.NewSnapshotServiceImplWithStore(cfg, store, nil)
	metadata := ebsmetadata.NewStore(store, "test-bucket")
	for i := range n {
		id := "snap-fanout" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		require.NoError(t, metadata.PutSnapshot(context.Background(), ebsmetadata.Snapshot{
			SnapshotID: id, VolumeID: "vol-fanout", State: "completed",
			OwnerID: fanoutAccountID, StartTime: time.Now(),
		}))
	}
	return svc
}

// TestDescribeSnapshotsReadsMetadataConcurrently pins the fix for a describe
// whose latency was the sum of every snapshot's metadata read: 24 snapshots at
// 100ms each took 2.4s serially, which is enough to miss the caller's timeout
// on an account that has done nothing wrong.
func TestDescribeSnapshotsReadsMetadataConcurrently(t *testing.T) {
	store := &slowMetadataStore{ObjectStore: objectstore.NewMemoryObjectStore(), delay: 100 * time.Millisecond}
	svc := seedSnapshots(t, store, 24)

	start := time.Now()
	out, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{}, fanoutAccountID)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Len(t, out.Snapshots, 24)
	assert.Greater(t, store.peak, 1, "metadata reads must overlap, not run one after the other")
	assert.Less(t, elapsed, time.Second,
		"a describe must cost about the slowest read, not the sum of them all")
}

// TestDescribeSnapshotsReportsAnExpiredDeadline covers the case that made this
// visible in production: when the caller has already given up, every metadata
// read fails, and an empty snapshot list would read as "this account has none".
func TestDescribeSnapshotsReportsAnExpiredDeadline(t *testing.T) {
	store := &slowMetadataStore{ObjectStore: objectstore.NewMemoryObjectStore(), delay: time.Second}
	svc := seedSnapshots(t, store, 4)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	out, err := svc.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{}, fanoutAccountID)
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error(),
		"a describe that ran out of time must not answer with a short list")
	assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
}
