//go:build e2e

// Backend byte accounting for the E2E teardown path.
//
// DeleteVolume purges a volume's entire backend prefix -- including any
// chunks a leak left behind -- before anything measures it. That is the
// same masking that hid the storage-growth leak in production: a passing
// E2E cannot fail on backend growth if the evidence is gone before the
// assertion runs. This file snapshots a volume's backend chunk footprint
// over the S3 API BEFORE a caller purges it, so the teardown path can
// assert on it instead of merely being unable to prove there was nothing to
// see.
//
// # Relationship to scripts/vbrefscan
//
// vbrefscan (mulgadc/mulga, scripts/vbrefscan) is the authoritative tool for
// this class of measurement and already reads exactly the data this file
// starts from -- the total object bytes under a volume's chunks/ prefix --
// as the first step of its live-vs-garbage breakdown. It cannot be imported
// here: it lives in a separate git repository and Go module (the mulga
// monorepo, not the spinifex checkout the E2E suite builds from), and
// depending on it from spinifex would break a standalone spinifex checkout
// the same way tests/e2e/storagegrowth/main_test.go already documents and
// deliberately avoids for its own measurement.
//
// What is reused is the technique and the metric, not a second
// reimplementation of vbrefscan's block-map parsing: summing ListObjectsV2
// object sizes under "<volume>/chunks/" is exactly vbrefscan's own
// LiveChunkBytes step. This file stops there deliberately -- it does not
// decode the block map to split that total into referenced vs.
// fully-unreferenced vs. internally-garbage bytes, because the teardown
// assertion below only needs to know whether the backend footprint stayed
// within the RS-erasure-floor band of what the guest actually wrote, not
// which chunk is at fault. Attributing blame within a leak is vbrefscan's
// job on a retained volume; this is a pass/fail gate that runs on every
// volume, retained or not.
package harness

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
)

const (
	// backendAccountingBucketEnv overrides the predastore bucket EBS volumes
	// live under. Defaults to "predastore" -- the bucket name emitted by
	// cmd/spinifex/cmd/templates/predastore.toml's [nodes.<node>.predastore]
	// block for every node this suite provisions.
	backendAccountingBucketEnv = "SPINIFEX_PREDASTORE_BUCKET"
	defaultPredastoreBucket    = "predastore"

	// BackendByteAccountingTolerance is the ceiling ratio of backend chunk
	// bytes to guest-written logical bytes a healthy teardown may show.
	//
	// The floor is viperblock's own encoding overhead, not slack for a leak:
	// chunks are written under Reed-Solomon 2+1 erasure coding, which splits
	// each object into two data shards plus one parity shard of the same
	// size -- 1.5x the logical payload, not 3x replication -- and every
	// block is additionally framed with a 16-byte AES-GCM tag.
	//
	// That 1.5x floor is a precondition, not a universal constant. It holds
	// because this suite provisions RS(2,1): cmd/spinifex/cmd/templates/
	// predastore.toml's [rs] block defaults to data=2, parity=1. The same
	// template documents RS(3,2) as the production recommendation, whose
	// floor is 5/3 = ~1.667x and would leave only ~12% headroom under the
	// ceiling below instead of the ~25% intended. Recompute this value if the
	// suite is ever pointed at a different RS profile rather than assuming
	// the band still holds.
	//
	// A healthy, fully-GC'd volume settles at ~1.5x and does not go
	// materially above it. The tolerance widens that floor by 25% (to 1.875x) to
	// absorb two effects that are real but not leaks: every chunk pays a
	// fixed header regardless of how many blocks it packs (pulling a
	// sparse volume's ratio up), and the tail chunk of any write run is
	// under-packed by construction (a chunk flushes on full or on drain,
	// so the last one of a run is real, unavoidable overhead). Both shrink
	// as a proportion of the total as the workload's footprint grows, so a
	// large workload should sit close to the bare floor and a small one
	// gets the extra headroom it actually needs.
	//
	// This does not attempt to catch the slow, bounded residual that a
	// future partial-chunk compaction pass would close -- only the
	// unbounded-growth shape the storage-growth incident produced, where
	// a leak multiplies with every overwrite pass instead of settling.
	BackendByteAccountingTolerance = 1.875
)

// BackendVolumeBytes is a volume's backend chunk footprint at a point in
// time: the same "live chunk bytes" total vbrefscan reports as its first
// metric, measured the same way (summing ListObjectsV2 object sizes under
// the volume's chunks/ prefix). It says nothing about which bytes are still
// referenced by the volume's live block map -- it is simply the number
// DeleteVolume is about to purge.
type BackendVolumeBytes struct {
	VolumeID   string
	ChunkCount int
	TotalBytes int64
}

// SnapshotVolumeBackendBytes sums the backend chunk footprint for volID
// under bucket on the predastore S3 endpoint at host. Call this BEFORE
// DeleteVolume: purge deletes the objects being counted, and a snapshot
// taken after cannot see what was there.
func SnapshotVolumeBackendBytes(ctx context.Context, host, bucket, volID string) (BackendVolumeBytes, error) {
	if volID == "" {
		return BackendVolumeBytes{}, fmt.Errorf("backend byte snapshot: empty volume id")
	}
	cli, err := newPredastoreS3(host)
	if err != nil {
		return BackendVolumeBytes{}, fmt.Errorf("backend byte snapshot: %w", err)
	}

	prefix := volID + "/chunks/"
	result := BackendVolumeBytes{VolumeID: volID}
	var token *string
	for {
		out, lerr := cli.ListObjectsV2WithContext(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if lerr != nil {
			return BackendVolumeBytes{}, fmt.Errorf("backend byte snapshot: list %s/%s: %w", bucket, prefix, lerr)
		}
		for _, obj := range out.Contents {
			if obj.Size == nil {
				continue
			}
			result.TotalBytes += *obj.Size
			result.ChunkCount++
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}
	return result, nil
}

// AssertBackendByteAccounting fails t immediately if snap's backend chunk
// footprint exceeds guestWrittenBytes by more than
// BackendByteAccountingTolerance. Call with a snapshot taken before the
// volume is purged -- purge deletes the evidence this asserts on. Intended
// for direct use in a test body (e.g. a workload that knows exactly how many
// bytes it wrote); the shared EnsureVolume teardown hook uses the non-fatal
// twin below so a violation does not also skip the DeleteVolume call after it.
func AssertBackendByteAccounting(t *testing.T, snap BackendVolumeBytes, guestWrittenBytes int64) {
	t.Helper()
	if msg, bad := backendByteAccountingViolation(snap, guestWrittenBytes); bad {
		t.Fatalf("%s", msg)
	}
	Detail(t, "backend_bytes", snap.TotalBytes, "guest_written_bytes", guestWrittenBytes,
		"ratio", fmt.Sprintf("%.3f", float64(snap.TotalBytes)/float64(guestWrittenBytes)), "chunk_count", snap.ChunkCount)
}

// checkBackendByteAccounting is the non-fatal form used from EnsureVolume's
// cleanup callback. t.Errorf marks the test failed without unwinding the
// goroutine the way t.Fatalf (runtime.Goexit) would -- inside a t.Cleanup
// callback, Goexit would abort the DeleteVolume call immediately below it
// and any sibling cleanups still queued behind this one, turning a reported
// leak into a real one.
func checkBackendByteAccounting(t *testing.T, snap BackendVolumeBytes, guestWrittenBytes int64) {
	t.Helper()
	if msg, bad := backendByteAccountingViolation(snap, guestWrittenBytes); bad {
		t.Errorf("%s", msg)
		return
	}
	Detail(t, "backend_bytes", snap.TotalBytes, "guest_written_bytes", guestWrittenBytes,
		"ratio", fmt.Sprintf("%.3f", float64(snap.TotalBytes)/float64(guestWrittenBytes)), "chunk_count", snap.ChunkCount)
}

// backendByteAccountingViolation holds the one ratio computation both the
// fatal and non-fatal assertion forms share, so the tolerance logic and its
// message exist in exactly one place.
func backendByteAccountingViolation(snap BackendVolumeBytes, guestWrittenBytes int64) (string, bool) {
	if guestWrittenBytes <= 0 {
		return fmt.Sprintf("backend byte accounting: guestWrittenBytes must be positive, got %d for volume %s -- the denominator every ratio divides by is unusable",
			guestWrittenBytes, snap.VolumeID), true
	}
	ratio := float64(snap.TotalBytes) / float64(guestWrittenBytes)
	if ratio > BackendByteAccountingTolerance {
		return fmt.Sprintf("backend byte accounting: volume %s backend chunk bytes %d is %.2fx guest-written %d bytes (%d chunks) -- exceeds the %.3fx RS-erasure-floor tolerance; backend growth looks unbounded, not settled at the floor",
			snap.VolumeID, snap.TotalBytes, ratio, guestWrittenBytes, snap.ChunkCount, BackendByteAccountingTolerance), true
	}
	return "", false
}

// RegisterVolumeGuestBytes records how many bytes a test actually wrote to
// volID's guest filesystem, for the teardown byte-accounting check
// EnsureVolume runs before purge. Optional: a volume with no registered
// baseline is still snapshotted and logged before purge (visibility), just
// not asserted against. Most EnsureVolume callers never attach or write to
// the volume they created, so there is nothing to compare against until a
// test actually knows what should be there.
func (f *Fixture) RegisterVolumeGuestBytes(volID string, guestWrittenBytes int64) {
	f.volumeGuestBytesMu.Lock()
	defer f.volumeGuestBytesMu.Unlock()
	if f.volumeGuestBytes == nil {
		f.volumeGuestBytes = map[string]int64{}
	}
	f.volumeGuestBytes[volID] = guestWrittenBytes
}

func (f *Fixture) volumeGuestBytesFor(volID string) (int64, bool) {
	f.volumeGuestBytesMu.Lock()
	defer f.volumeGuestBytesMu.Unlock()
	b, ok := f.volumeGuestBytes[volID]
	return b, ok
}

// snapshotVolumeBackendBytesBeforePurge is EnsureVolume's teardown hook: it
// runs immediately before DeleteVolume, for every volume EnsureVolume ever
// creates, closing the "purge before measurement" blind spot for every live
// test at once rather than in one dedicated suite.
//
// It always snapshots and logs the volume's backend chunk footprint when a
// predastore endpoint can be resolved from the environment -- pure
// visibility, matching the best-effort style of tagRunResources elsewhere in
// this file's neighbourhood. It additionally hard-checks the footprint
// against a guest-written-bytes baseline IF the calling test registered one
// via RegisterVolumeGuestBytes; most callers never attach or write to the
// volume they created, so there is usually nothing to check against, and
// this stays silent (beyond the log line) rather than guessing a baseline.
//
// Best-effort like the rest of Ensure*'s teardown plumbing: a resolution or
// snapshot failure is logged, never fatal, and never blocks DeleteVolume --
// this check must not become a second way for a live suite to flake.
func snapshotVolumeBackendBytesBeforePurge(t *testing.T, fx *Fixture, volID string) {
	t.Helper()
	host := resolveBackendAccountingHost()
	if host == "" {
		return
	}
	bucket := getenv(backendAccountingBucketEnv, defaultPredastoreBucket)

	snap, err := SnapshotVolumeBackendBytes(context.Background(), host, bucket, volID)
	if err != nil {
		t.Logf("backend byte accounting: snapshot %s before purge: %v", volID, err)
		return
	}
	t.Logf("backend byte accounting: volume %s backend chunk bytes=%d chunks=%d (snapshotted before purge)",
		volID, snap.TotalBytes, snap.ChunkCount)

	if guestBytes, ok := fx.volumeGuestBytesFor(volID); ok {
		checkBackendByteAccounting(t, snap, guestBytes)
	}
}

// resolveBackendAccountingHost returns the host to reach predastore's S3
// endpoint directly for a byte-accounting snapshot, or "" if it cannot be
// determined from the environment. Mirrors LoadEnv's WANHost resolution
// (env.go) without its t.Skip side effect: this runs from inside a
// t.Cleanup callback, where skipping the test is meaningless and would
// panic rather than skip cleanly.
func resolveBackendAccountingHost() string {
	if h := getenv("SPINIFEX_WAN_IP", ""); h != "" {
		return h
	}
	configDir := getenv("SPINIFEX_CONFIG_DIR", "")
	if configDir == "" {
		for _, c := range []string{"/etc/spinifex", os.ExpandEnv("$HOME/spinifex/config")} {
			if stat, err := os.Stat(c); err == nil && stat.IsDir() {
				configDir = c
				break
			}
		}
	}
	if h := advertiseIP(configDir); h != "" {
		return h
	}
	for _, ip := range discoverSingleNodeIPs() {
		if !strings.HasPrefix(ip, "127.") {
			return ip
		}
	}
	return ""
}
