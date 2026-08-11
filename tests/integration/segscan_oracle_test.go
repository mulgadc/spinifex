//go:build integration && segscanoracle

package integration

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/testutil/segscanoracle"
	testpredastore "github.com/mulgadc/spinifex/tests/fixtures/predastore"
	"github.com/stretchr/testify/require"
)

// segscanOracleNode is the fixture node this test scans. The fixture's RS
// config is data=3/parity=2 across exactly 5 blob nodes (see
// tests/fixtures/predastore's topology), so every PutObject places exactly
// one shard — one live extent — on every one of them, node-2 included.
const segscanOracleNode = "node-2"

// TestSegscanOracle_PutOverwriteDelete drives real S3 writes against the
// predastore fixture and asserts scripts/segscan's live / dead-tombstoned /
// dead-orphan byte accounting for one node's data dir against expectations
// derived independently of segscan's own decoder.
//
// "Independent" here means: not re-deriving segscan's expected byte totals
// by hand-parsing the same .seg/.idx on-disk format segscan parses (that
// would just be a second copy of the same decoder, prone to drifting from
// the real format in lockstep with segscan itself, proving nothing). Instead
// the expectations come from predastore's own documented Store contract
// (store/store.go's commitExtent and Delete both write a tombstone for any
// extent they supersede, in the same transaction): an overwrite or delete
// must move an extent's bytes from live to dead-tombstoned, byte for byte,
// and must never produce dead-orphan bytes. That is a real invariant of the
// production write path, not an assumption about segscan's own parsing, so
// this test still fails loudly if segscan's decode ever drifts from what
// predastore actually persists.
func TestSegscanOracle_PutOverwriteDelete(t *testing.T) {
	fixture := testpredastore.Start(t)
	cli := predastoreS3Client(t, fixture)

	bucket := fmt.Sprintf("segscan-oracle-%d", time.Now().UnixNano())
	_, err := cli.CreateBucket(&s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err, "create bucket %s", bucket)
	t.Cleanup(func() {
		_, _ = cli.DeleteBucket(&s3.DeleteBucketInput{Bucket: aws.String(bucket)})
	})

	nodeDir := filepath.Join(fixture.DataDir, segscanOracleNode)
	const key = "segscan-oracle/object"

	scan := func() *segscanoracle.Report {
		t.Helper()
		// Give an async write a moment to settle before copying: PutObject's
		// QUIC round trip to the shard nodes completes before it returns, but
		// this leaves a small margin against a torn on-disk read racing the
		// daemon's own fsync, rather than relying on that timing exactly.
		time.Sleep(200 * time.Millisecond)
		copyDir := segscanoracle.CopyNodeDir(t, nodeDir)
		return segscanoracle.Run(t, copyDir)
	}

	baseline := scan()

	// Put: a fresh object adds exactly one new live extent on this node, and
	// must not touch dead-tombstoned or dead-orphan totals.
	payloadA := bytes.Repeat([]byte("a"), 100<<10) // 100 KiB
	_, err = cli.PutObject(&s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(payloadA)})
	require.NoError(t, err, "put %s", key)

	afterPut := scan()
	require.Equal(t, baseline.Totals.DeadOrphan, afterPut.Totals.DeadOrphan, "put must not create dead-orphan bytes")
	require.Equal(t, baseline.Totals.DeadTombstoned, afterPut.Totals.DeadTombstoned, "put must not create dead-tombstoned bytes (nothing superseded yet)")
	require.Greater(t, afterPut.Totals.LivePhysical, baseline.Totals.LivePhysical, "put must grow live physical bytes")
	liveDeltaA := afterPut.Totals.LivePhysical - baseline.Totals.LivePhysical

	// Overwrite with a different-sized payload: the old version's extent must
	// become dead-tombstoned with exactly its prior live byte count -- not
	// dropped, not double-counted, and not reclassified as orphan.
	payloadA2 := bytes.Repeat([]byte("b"), 500<<10) // 500 KiB
	_, err = cli.PutObject(&s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(payloadA2)})
	require.NoError(t, err, "overwrite %s", key)

	afterOverwrite := scan()
	require.Equal(t, baseline.Totals.DeadOrphan, afterOverwrite.Totals.DeadOrphan, "overwrite must not create dead-orphan bytes")
	require.Equal(t, liveDeltaA, afterOverwrite.Totals.DeadTombstoned-baseline.Totals.DeadTombstoned,
		"overwrite must tombstone exactly the superseded version's live physical bytes")
	require.Greater(t, afterOverwrite.Totals.LivePhysical, baseline.Totals.LivePhysical, "overwrite must leave the new version live")
	liveDeltaA2 := afterOverwrite.Totals.LivePhysical - baseline.Totals.LivePhysical

	// Delete: the current version's extent must also become dead-tombstoned
	// (predastore's Delete writes a tombstone too, same as an overwrite's
	// supersede path) -- not dead-orphan -- and live totals return to baseline.
	_, err = cli.DeleteObject(&s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	require.NoError(t, err, "delete %s", key)

	afterDelete := scan()
	require.Equal(t, baseline.Totals.DeadOrphan, afterDelete.Totals.DeadOrphan, "delete must not create dead-orphan bytes")
	require.Equal(t, liveDeltaA2, afterDelete.Totals.DeadTombstoned-afterOverwrite.Totals.DeadTombstoned,
		"delete must tombstone exactly the live version's physical bytes")
	require.Equal(t, baseline.Totals.LivePhysical, afterDelete.Totals.LivePhysical, "live physical bytes must return to baseline once the object is gone")
	require.Equal(t, baseline.Totals.LiveLogical, afterDelete.Totals.LiveLogical, "live logical bytes must return to baseline once the object is gone")
}
