package volume

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// peakGetStore delays every read, counts the reads started and records the
// most in flight at once, so a test can see how far one request fans out.
// A key containing failKey fails with a store error that is not NoSuchKey.
type peakGetStore struct {
	objectstore.ObjectStore

	delay    time.Duration
	failKey  string
	started  atomic.Int64
	inFlight atomic.Int64
	highest  atomic.Int64
}

func (s *peakGetStore) GetObject(ctx context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	s.started.Add(1)
	now := s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	for {
		high := s.highest.Load()
		if now <= high || s.highest.CompareAndSwap(high, now) {
			break
		}
	}
	time.Sleep(s.delay)
	if s.failKey != "" && strings.Contains(aws.StringValue(in.Key), s.failKey) {
		return nil, errors.New("predastore: connection reset")
	}
	return s.ObjectStore.GetObject(ctx, in)
}

// describeByIDs runs one member of the DescribeVolumes family for ids and
// returns the volume IDs it answered with, in response order.
type describeByIDs func(ctx context.Context, svc *VolumeServiceImpl, ids []*string) ([]string, error)

var describeFamily = []struct {
	name string
	call describeByIDs
	// storeFailure is the error code a store failure that is not NoSuchKey
	// gives today. The modifications path maps every failure to not-found.
	storeFailure string
}{
	{"DescribeVolumes", func(ctx context.Context, svc *VolumeServiceImpl, ids []*string) ([]string, error) {
		out, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: ids}, testVolAccountID)
		if err != nil {
			return nil, err
		}
		got := make([]string, 0, len(out.Volumes))
		for _, v := range out.Volumes {
			got = append(got, aws.StringValue(v.VolumeId))
		}
		return got, nil
	}, awserrors.ErrorServerInternal},
	{"DescribeVolumeStatus", func(ctx context.Context, svc *VolumeServiceImpl, ids []*string) ([]string, error) {
		out, err := svc.DescribeVolumeStatus(ctx, &ec2.DescribeVolumeStatusInput{VolumeIds: ids}, testVolAccountID)
		if err != nil {
			return nil, err
		}
		got := make([]string, 0, len(out.VolumeStatuses))
		for _, v := range out.VolumeStatuses {
			got = append(got, aws.StringValue(v.VolumeId))
		}
		return got, nil
	}, awserrors.ErrorServerInternal},
	{"DescribeVolumesModifications", func(ctx context.Context, svc *VolumeServiceImpl, ids []*string) ([]string, error) {
		out, err := svc.DescribeVolumesModifications(ctx, &ec2.DescribeVolumesModificationsInput{VolumeIds: ids}, testVolAccountID)
		if err != nil {
			return nil, err
		}
		got := make([]string, 0, len(out.VolumesModifications))
		for _, v := range out.VolumesModifications {
			got = append(got, aws.StringValue(v.VolumeId))
		}
		return got, nil
	}, awserrors.ErrorInvalidVolumeNotFound},
}

// seedFanoutVolumes writes n volume documents, each with a modification record
// so every member of the family answers for it. The IDs are returned in an
// order that is not sorted, because every response is sorted by volume ID.
func seedFanoutVolumes(t *testing.T, store objectstore.ObjectStore, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("vol-%017x", n-i)
		seedVolumeDocument(t, store, ebsmetadata.Volume{
			VolumeID: ids[i], State: "available", CapacityGiB: 8, AvailabilityZone: "ap-southeast-2a",
			VolumeType: "gp3", CreatedAt: time.Now(),
			Modification: &ebsmetadata.VolumeModification{ModificationState: "completed", Progress: 100, OriginalSize: 8, TargetSize: 8},
		})
	}
	return ids
}

func sortedCopy(ids []string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

func newFanoutService(t *testing.T, store objectstore.ObjectStore) *VolumeServiceImpl {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { slog.SetDefault(prev) })
	cfg := &config.Config{AZ: "ap-southeast-2a", Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	return NewVolumeServiceImplWithStore(cfg, store, nil)
}

func TestDescribeVolumesFamily_BoundsConcurrentDocumentReads(t *testing.T) {
	for _, m := range describeFamily {
		t.Run(m.name, func(t *testing.T) {
			mem := objectstore.NewMemoryObjectStore()
			ids := seedFanoutVolumes(t, mem, 4*ebsmetadata.FetchConcurrency)
			store := &peakGetStore{ObjectStore: mem, delay: 20 * time.Millisecond}
			svc := newFanoutService(t, store)

			got, err := m.call(context.Background(), svc, aws.StringSlice(ids))
			require.NoError(t, err)

			assert.LessOrEqual(t, store.highest.Load(), int64(ebsmetadata.FetchConcurrency),
				"one request had more document reads in flight than the shared bound")
			assert.Equal(t, sortedCopy(ids), got, "every requested volume, sorted by ID")
		})
	}
}

// A request whose deadline has passed must stop starting reads, or a large
// ID list keeps loading the object store after its caller has gone.
func TestDescribeVolumesFamily_StopsStartingReadsAtTheDeadline(t *testing.T) {
	for _, m := range describeFamily {
		t.Run(m.name, func(t *testing.T) {
			mem := objectstore.NewMemoryObjectStore()
			ids := seedFanoutVolumes(t, mem, 8*ebsmetadata.FetchConcurrency)
			store := &peakGetStore{ObjectStore: mem, delay: 40 * time.Millisecond}
			svc := newFanoutService(t, store)

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
			defer cancel()
			_, err := m.call(ctx, svc, aws.StringSlice(ids))

			assert.Error(t, err, "a request cut off by its deadline must not answer as complete")
			assert.LessOrEqual(t, store.started.Load(), int64(2*ebsmetadata.FetchConcurrency),
				"reads kept starting after the deadline")
		})
	}
}

// The bound must not change what each ID answers.
func TestDescribeVolumesFamily_KeepsPerIDResults(t *testing.T) {
	for _, m := range describeFamily {
		t.Run(m.name, func(t *testing.T) {
			mem := objectstore.NewMemoryObjectStore()
			ids := seedFanoutVolumes(t, mem, 3)
			store := &peakGetStore{ObjectStore: mem, failKey: ids[2]}
			svc := newFanoutService(t, store)

			got, err := m.call(context.Background(), svc, []*string{aws.String(ids[0]), nil, aws.String(ids[1])})
			require.NoError(t, err)
			assert.Equal(t, sortedCopy(ids[:2]), got, "a nil entry is skipped")

			_, err = m.call(context.Background(), svc, []*string{aws.String(ids[0]), aws.String("vol-0000000000000dead")})
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error(), "an unknown ID")

			_, err = m.call(context.Background(), svc, []*string{aws.String(ids[0]), aws.String(ids[2])})
			require.Error(t, err)
			assert.Equal(t, m.storeFailure, err.Error(), "a store failure that is not NoSuchKey")
		})
	}
}

// deadlineGetStore records the deadline of the context each read runs under.
type deadlineGetStore struct {
	objectstore.ObjectStore

	mu        sync.Mutex
	deadlines []time.Time
	missing   int
}

func (s *deadlineGetStore) GetObject(ctx context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	s.mu.Lock()
	if d, ok := ctx.Deadline(); ok {
		s.deadlines = append(s.deadlines, d)
	} else {
		s.missing++
	}
	s.mu.Unlock()
	return s.ObjectStore.GetObject(ctx, in)
}

// The daemon handler has no deadline of its own, so the reads must carry the
// daemon-local read budget, measured from when the service is called. It does
// not cover time spent in the gateway or on NATS before the call.
func TestDescribeVolumesFamily_ReadsCarryTheDaemonReadBudget(t *testing.T) {
	const readBudget = 25 * time.Second
	for _, m := range describeFamily {
		t.Run(m.name, func(t *testing.T) {
			mem := objectstore.NewMemoryObjectStore()
			ids := seedFanoutVolumes(t, mem, 3)
			store := &deadlineGetStore{ObjectStore: mem}
			svc := newFanoutService(t, store)

			start := time.Now()
			_, err := m.call(context.Background(), svc, aws.StringSlice(ids))
			require.NoError(t, err)

			store.mu.Lock()
			defer store.mu.Unlock()
			assert.Zero(t, store.missing, "a read ran with no deadline")
			require.Len(t, store.deadlines, len(ids))
			for _, d := range store.deadlines {
				assert.False(t, d.After(time.Now().Add(readBudget)), "read deadline %v after the call is past the daemon read budget", d.Sub(start))
			}
		})
	}
}

// gateGetStore holds the first batch of reads open until the test releases
// them, and records any read that starts after its context was cancelled.
type gateGetStore struct {
	objectstore.ObjectStore

	batch      int
	firstBatch chan struct{}
	release    chan struct{}
	started    atomic.Int64
	late       atomic.Int64
}

func (s *gateGetStore) GetObject(ctx context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if ctx.Err() != nil {
		s.late.Add(1)
	}
	if n := s.started.Add(1); n <= int64(s.batch) {
		if n == int64(s.batch) {
			close(s.firstBatch)
		}
		<-s.release
	}
	return s.ObjectStore.GetObject(ctx, in)
}

// Once the request's context ends, no read may start, and the IDs left unread
// fail as a server error: they were never looked up, so they are not missing.
func TestDescribeVolumesFamily_StartsNoReadAfterCancel(t *testing.T) {
	for _, m := range describeFamily {
		t.Run(m.name, func(t *testing.T) {
			mem := objectstore.NewMemoryObjectStore()
			ids := seedFanoutVolumes(t, mem, 4*ebsmetadata.FetchConcurrency)
			store := &gateGetStore{ObjectStore: mem, batch: ebsmetadata.FetchConcurrency,
				firstBatch: make(chan struct{}), release: make(chan struct{})}
			svc := newFanoutService(t, store)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := m.call(ctx, svc, aws.StringSlice(ids))
				done <- err
			}()
			<-store.firstBatch
			cancel()
			close(store.release)
			err := <-done

			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorServerInternal, err.Error(), "unread IDs are a server error, not a missing volume")
			assert.Zero(t, store.late.Load(), "a read started after the context was cancelled")
			assert.Equal(t, int64(ebsmetadata.FetchConcurrency), store.started.Load(), "reads started")
		})
	}
}
