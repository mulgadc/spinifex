// Package vbscan is a read-only storage-state oracle for viperblock volumes,
// driven directly over S3 rather than through any spinifex handler. Tests
// that only check "the handler did not error and the volume reloads" prove
// nothing about whether the bytes viperblock actually persisted are what was
// claimed — DescribeVolumes rebuilds its response from the same VolumeConfig
// fields a broken write could still populate correctly. vbscan reads the
// backend's own persisted state independently, so a test using it fails when
// storage itself is wrong even if every handler-level response still looks
// fine.
//
// This mirrors scripts/vbrefscan (in the mulga umbrella module) but is
// reimplemented locally rather than imported: spinifex cannot depend on
// mulga, the module that contains it, without a repo-level cycle. It is
// deliberately its own leaf package, not part of the shared spinifex/testutil
// package that ~80 test files import — see tests/fixtures/predastore's package
// comment for why that matters (goleak poisoning via a transitively-linked
// init-time goroutine). vbscan does not import predastore/quic/quicclient,
// directly or transitively, and must stay that way.
//
// # Scope: no chunk-reference-integrity scan
//
// vbrefscan also cross-checks every chunk object against the volume's
// checkpointed block map (checkpoints/blocks.live.bin) to report unreferenced
// bytes, via viperblock's exported ParseBlockCheckpointBytes. That function
// does not exist in the viperblock version spinifex currently depends on
// (v1.12.1, confirmed by extracting the tagged module and grepping it) —
// it is a newer, unreleased addition. Reimplementing that decode locally
// would mean hand-rolling a private wire format spinifex has no exported
// access to, which is exactly the drift hazard segscan carries and which
// this package must not take on without the same drift guard segscan needs.
// So vbscan reports only what v1.12.1's public API can read soundly: the
// persisted VBState (config.json) and the real chunk objects present under
// the volume's prefix. That is also all a volume with no write I/O has to
// show: CreateVolume never calls SaveLiveCheckpoint, so a freshly created
// volume has no checkpoint to scan regardless of viperblock version — the
// missing function costs nothing for that case, only for auditing a volume
// with real write history, which is out of scope here.
package vbscan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/viperblock/viperblock"
)

// ObjectStore is the slice of the S3 API vbscan needs. Narrowed to two
// read-only verbs, matching scripts/vbrefscan's objectStore, so the oracle
// cannot mutate the volume it measures even by mistake.
type ObjectStore interface {
	GetObject(ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error)
	ListObjectsV2(ctx context.Context, input *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error)
}

// objectstore.S3ObjectStore is the concrete store every real-predastore test
// passes in; this check confirms it satisfies the narrowed interface above
// (it also implements PutObject/DeleteObject/EnsureBucket, none of which
// ObjectStore exposes).
var _ ObjectStore = (*objectstore.S3ObjectStore)(nil)

// chunkKeyRe matches "<volume>/chunks/chunk.<id>.bin" and captures the id,
// mirroring the key format viperblock's types.GetFilePath(FileTypeChunk, ...)
// writes. The id is zero-padded to 8 digits by that formatter but not bounded
// to 8 digits once a volume exceeds 10^8 chunks, so the capture is greedy.
var chunkKeyRe = regexp.MustCompile(`^chunks/chunk\.(\d+)\.bin$`)

// Report is one volume's real persisted storage state, read independently of
// any handler's own view of it.
type Report struct {
	// Volume is the scanned volume ID.
	Volume string
	// State is the full VBState decoded from the volume's persisted
	// config.json — the same struct viperblock itself writes and reads,
	// not a hand-rolled subset, so no local copy of the format can drift
	// from what production code emits.
	State viperblock.VBState
	// LiveChunkCount and LiveChunkBytes are the real chunk objects present
	// under this volume's chunks/ prefix — ground truth for "how much data
	// exists", independent of ObjectNum or any in-memory accounting.
	LiveChunkCount int
	LiveChunkBytes int64
}

// Scanner holds the resolved inputs for scanning one bucket's volumes.
type Scanner struct {
	store  ObjectStore
	bucket string
}

// NewScanner builds a Scanner against store/bucket. store is typically
// objectstore.NewS3ObjectStoreFromConfig pointed at the same real predastore
// fixture the handler under test writes to.
func NewScanner(store ObjectStore, bucket string) *Scanner {
	return &Scanner{store: store, bucket: bucket}
}

// Inspect reads volume's persisted state and chunk objects directly from S3.
// It performs no reference-integrity cross-check (see the package comment)
// and asserts no invariants itself — callers decide what the persisted state
// should look like for their scenario.
func (s *Scanner) Inspect(ctx context.Context, volume string) (*Report, error) {
	state, err := s.readState(ctx, volume)
	if err != nil {
		return nil, err
	}

	count, size, err := s.liveChunks(ctx, volume)
	if err != nil {
		return nil, err
	}

	return &Report{
		Volume:         volume,
		State:          *state,
		LiveChunkCount: count,
		LiveChunkBytes: size,
	}, nil
}

// readState fetches and decodes volume/config.json.
func (s *Scanner) readState(ctx context.Context, volume string) (*viperblock.VBState, error) {
	key := volume + "/config.json"
	out, err := s.store.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	defer out.Body.Close()

	raw, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}

	// StateBody unwraps the at-rest encryption envelope, a no-op for
	// unencrypted volumes; the block map and metadata stay plaintext even
	// on encrypted volumes, so no master key is needed here.
	var state viperblock.VBState
	if err := json.NewDecoder(bytes.NewReader(viperblock.StateBody(raw))).Decode(&state); err != nil {
		return nil, fmt.Errorf("parse %s: %w", key, err)
	}
	return &state, nil
}

// liveChunks lists volume's chunk objects, returning the count and total
// bytes reported by ListObjectsV2 (logical object size, not decoded).
func (s *Scanner) liveChunks(ctx context.Context, volume string) (int, int64, error) {
	prefix := volume + "/chunks/"
	var count int
	var size int64
	var token *string
	for {
		out, err := s.store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return 0, 0, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, obj := range out.Contents {
			if obj.Key == nil || obj.Size == nil {
				continue
			}
			rel := strings.TrimPrefix(*obj.Key, volume+"/")
			if m := chunkKeyRe.FindStringSubmatch(rel); m != nil {
				if _, err := strconv.ParseUint(m[1], 10, 64); err != nil {
					return 0, 0, fmt.Errorf("chunk key %q: %w", *obj.Key, err)
				}
				count++
				size += *obj.Size
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}
	return count, size, nil
}
