//go:build e2e

package harness

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

// Deployed predastore base path (SPINIFEX_PREDASTORE_BASE_PATH in the systemd
// unit). The compactor reclaims .seg/.idx files under the shard-store node dirs;
// the badger index under <base>/db is deliberately excluded.
const (
	predastoreBasePathDefault = "/var/lib/spinifex/predastore"
	shardNodesGlob            = "distributed/nodes/node-*"
	churnKeyPrefix            = "compaction-churn/"

	// duSentinel terminates the remote du command. Its presence in the combined
	// output proves the command ran to completion and the output made it back
	// intact, letting the harness tell a genuinely empty measurement apart from
	// output lost in SSH retrieval.
	duSentinel = "__SPX_DU_OK__"

	// churnBucketPrefix names the per-run bucket the churn creates. A dedicated
	// bucket the harness identity owns sidesteps the config-defined system bucket,
	// which belongs to a different account and rejects cross-account writes.
	churnBucketPrefix = "compaction-churn-"

	// compactionGateRatio: end-of-churn usage must fall below this fraction of
	// the churn peak for the gate to pass.
	compactionGateRatio = 0.70
)

// Churn tuning. Sized for the 30s E2E compaction interval; validate against a
// real run before relying on the margin. churnBaselineMultiple makes the write
// volume dominate the non-deletable baseline so the 70% gate clears; the floor
// keeps the gate meaningful when a scenario leaves little pre-existing data.
const (
	churnObjectBytes      = 1 << 20 // 1 MiB per object
	churnMinObjects       = 64
	churnMinBytes         = int64(churnMinObjects) * churnObjectBytes
	churnBaselineMultiple = 3

	settleDeadline     = 3 * time.Minute
	settlePollInterval = 10 * time.Second
)

// predastoreBasePath resolves the deployed base path, allowing override via the
// same env var the systemd unit sets so the du glob stays correct off-default.
func predastoreBasePath() string {
	if v := os.Getenv("SPINIFEX_PREDASTORE_BASE_PATH"); v != "" {
		return v
	}
	return predastoreBasePathDefault
}

// AssertPredastoreCompaction drives a deliberate write-then-delete churn against
// the cluster's predastore S3 endpoint, brackets it with on-disk shard-store
// usage measurements, and asserts end-of-churn usage settles below
// compactionGateRatio of the churn peak.
func AssertPredastoreCompaction(ctx context.Context, t *testing.T, cluster *Cluster) {
	t.Helper()
	Phase(t, "Predastore — Compaction Reclaim Gate")

	ssh, err := NewSSH(cluster)
	if err != nil {
		t.Fatalf("compaction: ssh transport: %v", err)
	}
	defer func() { _ = ssh.Close() }()

	cli, err := newPredastoreS3(cluster)
	if err != nil {
		t.Fatalf("compaction: s3 client: %v", err)
	}
	bucket := fmt.Sprintf("%s%d", churnBucketPrefix, time.Now().UnixNano())
	if _, err := cli.CreateBucketWithContext(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("compaction: create bucket %s: %v", bucket, err)
	}
	t.Cleanup(func() {
		if _, err := cli.DeleteBucket(&s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Logf("compaction: cleanup delete bucket %s: %v", bucket, err)
		}
	})

	baseline, err := shardStoreUsageBytes(ctx, ssh, cluster)
	if err != nil {
		t.Fatalf("compaction: baseline du: %v", err)
	}

	peak, err := churnPredastore(ctx, cli, bucket, ssh, cluster, baseline)
	if err != nil {
		t.Fatalf("compaction: churn: %v", err)
	}
	if peak <= baseline {
		t.Fatalf("compaction: peak %d not above baseline %d — churn produced no reclaimable space", peak, baseline)
	}

	target := int64(float64(peak) * compactionGateRatio)
	end, err := pollUntilSettled(ctx, ssh, cluster, target, settleDeadline, settlePollInterval)
	if err != nil {
		t.Fatalf("compaction: %v", err)
	}

	Detail(t, "baseline", baseline, "peak", peak, "target", target, "end", end)
}

// churnObjectCount sizes the churn batch so written volume dominates the
// non-deletable baseline (churnBaselineMultiple×) while never dropping below the
// floor, keeping the 70% gate meaningful regardless of pre-existing data.
func churnObjectCount(baseline int64) int {
	writeBytes := baseline * churnBaselineMultiple
	if writeBytes < churnMinBytes {
		writeBytes = churnMinBytes
	}
	count := int(writeBytes / churnObjectBytes)
	if count < churnMinObjects {
		count = churnMinObjects
	}
	return count
}

// shardStoreUsageBytes sums `du -sb` across every node's shard-store dirs over
// SSH, returning a single aggregate byte count. Excludes the badger index dir.
func shardStoreUsageBytes(ctx context.Context, ssh SSH, cluster *Cluster) (int64, error) {
	glob := filepath.Join(predastoreBasePath(), shardNodesGlob)
	cmd := fmt.Sprintf("du -sb %s 2>/dev/null | awk '{s+=$1} END{print s+0}'; echo %s", glob, duSentinel)

	var total int64
	for _, n := range cluster.Nodes {
		out, err := ssh.Run(ctx, n, cmd)
		if err != nil {
			return 0, fmt.Errorf("du on %s: %w", n.Name, err)
		}
		v, err := parseShardUsage(n.Name, out)
		if err != nil {
			return 0, err
		}
		total += v
	}
	return total, nil
}

// parseShardUsage extracts the byte count from the du+sentinel output. A missing
// sentinel means the output was truncated in transit (transport-layer drop). A
// present sentinel with no number means the remote measurement was genuinely
// empty, which counts as 0 — awk normally floors to "0", so empty is anomalous
// and logged for the next investigation.
func parseShardUsage(node string, out []byte) (int64, error) {
	s := string(out)
	if !strings.Contains(s, duSentinel) {
		return 0, fmt.Errorf("du on %s: sentinel %q absent, output lost in SSH retrieval (got %q)", node, duSentinel, s)
	}
	num := strings.TrimSpace(strings.Replace(s, duSentinel, "", 1))
	if num == "" {
		slog.Warn("compaction: empty du measurement, treating as 0", "node", node, "raw", s)
		return 0, nil
	}
	v, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("du parse on %s (%q): %w", node, num, err)
	}
	return v, nil
}

// churnPredastore writes then deletes a self-sized batch of objects under
// churnKeyPrefix in bucket, returning the peak shard-store usage measured at
// full-write before any delete.
func churnPredastore(ctx context.Context, cli *s3.S3, bucket string, ssh SSH, cluster *Cluster, baseline int64) (int64, error) {
	count := churnObjectCount(baseline)
	payload := bytes.Repeat([]byte("c"), churnObjectBytes)
	keys := make([]string, count)
	for i := range keys {
		keys[i] = fmt.Sprintf("%s%d", churnKeyPrefix, i)
		if _, err := cli.PutObjectWithContext(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(keys[i]),
			Body:   bytes.NewReader(payload),
		}); err != nil {
			return 0, fmt.Errorf("put %s: %w", keys[i], err)
		}
	}

	peak, err := shardStoreUsageBytes(ctx, ssh, cluster)
	if err != nil {
		return 0, err
	}

	for _, key := range keys {
		if _, err := cli.DeleteObjectWithContext(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		}); err != nil {
			return 0, fmt.Errorf("delete %s: %w", key, err)
		}
	}
	return peak, nil
}

// pollUntilSettled polls shard-store usage every interval until it drops below
// target or deadline elapses, returning the final reading.
func pollUntilSettled(ctx context.Context, ssh SSH, cluster *Cluster, target int64, deadline, interval time.Duration) (int64, error) {
	expired := time.After(deadline)
	tick := time.NewTicker(interval)
	defer tick.Stop()

	var last int64
	for {
		usage, err := shardStoreUsageBytes(ctx, ssh, cluster)
		if err != nil {
			return 0, err
		}
		last = usage
		if usage < target {
			return usage, nil
		}
		select {
		case <-expired:
			return last, fmt.Errorf("usage did not settle below %d within %s (last=%d)", target, deadline, last)
		case <-ctx.Done():
			return last, ctx.Err()
		case <-tick.C:
		}
	}
}

// newPredastoreS3 builds an S3 client pointed at a cluster node's predastore
// endpoint. The gateway (:9999) does not proxy S3 object operations, so churn
// targets predastore directly. Credentials resolve from SPINIFEX_AWS_* or the
// spinifex profile — the admin identity (AdministratorAccess), which owns and is
// authorized for the bucket the churn creates. TLS verification is skipped
// (test-only) since the assertion carries no Env for CA load.
func newPredastoreS3(cluster *Cluster) (*s3.S3, error) {
	if len(cluster.Nodes) == 0 {
		return nil, errors.New("compaction: cluster has no nodes")
	}
	endpoint := fmt.Sprintf("https://%s:%d", cluster.Nodes[0].Addr, predastoreHealthPort)
	cfg := &aws.Config{
		Endpoint:         aws.String(endpoint),
		Region:           aws.String(getenv("SPINIFEX_AWS_REGION", "ap-southeast-2")),
		S3ForcePathStyle: aws.Bool(true),
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test harness
			},
		},
	}
	opts := session.Options{Config: *cfg}
	if id, secret := os.Getenv("SPINIFEX_AWS_ACCESS_KEY_ID"), os.Getenv("SPINIFEX_AWS_SECRET_ACCESS_KEY"); id != "" && secret != "" {
		cfg.Credentials = credentials.NewStaticCredentials(id, secret, "")
		opts.Config = *cfg
	} else {
		opts.SharedConfigState = session.SharedConfigEnable
		opts.Profile = getenv("AWS_PROFILE", "spinifex")
	}
	sess, err := session.NewSessionWithOptions(opts)
	if err != nil {
		return nil, fmt.Errorf("compaction: s3 session: %w", err)
	}
	return s3.New(sess), nil
}
