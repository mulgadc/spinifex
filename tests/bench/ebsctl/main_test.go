//go:build e2e && bench

package ebsctl

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(m.Run())
}

// TestEBSControlPlaneLatency is the benchmark's single entry point, compiled
// to a standalone binary via `go test -tags "e2e bench" -c`. It measures
// per-operation API latency and settle time for CreateVolume, DescribeVolumes
// (single + list), CreateSnapshot, DeleteSnapshot, DeleteVolume, and
// (unless -attach-detach=false) AttachVolume/DetachVolume, then writes a
// RunResult JSON document.
func TestEBSControlPlaneLatency(t *testing.T) {
	env := harness.LoadEnv(t) // skips the test if SPINIFEX_E2E is unset
	if *flagIterations < 1 {
		t.Fatalf("-iterations must be >= 1, got %d", *flagIterations)
	}
	if *flagWarmup < 0 {
		t.Fatalf("-warmup must be >= 0, got %d", *flagWarmup)
	}
	if *flagConcurrency < 1 {
		t.Fatalf("-concurrency must be >= 1, got %d", *flagConcurrency)
	}

	awsCli := harness.NewAWSClient(t, env)
	fix := harness.NewFixture(t, awsCli)

	provider, source := DetectEBSProvider(t, env)
	t.Logf("ebs provider in effect: %s (source=%s)", provider, source)

	az := harness.DiscoverDefaultAZ(t, fix)
	runID := fmt.Sprintf("%d-%d", time.Now().Unix(), os.Getpid())
	tags := []*ec2.Tag{
		{Key: aws.String("Name"), Value: aws.String("ebsctl-bench")},
		{Key: aws.String("ebsctl:run"), Value: aws.String(runID)},
	}

	result := &RunResult{
		Meta: RunMeta{
			Timestamp:            time.Now().UTC(),
			GitSHA:               gitSHA(),
			Provider:             provider,
			ProviderSource:       source,
			NodeCount:            len(env.NodeIPs),
			IterationsPerWorker:  *flagIterations,
			WarmupPerWorker:      *flagWarmup,
			Concurrency:          *flagConcurrency,
			AttachDetachIncluded: *flagAttachDetach,
			VolumeSizeGiB:        *flagVolumeGiB,
		},
		Operations: map[string]*OpResult{},
	}

	t.Log("phase: CreateVolume / DeleteVolume")
	createRes, volIDs := runCreateVolume(t, awsCli, tags, az, *flagVolumeGiB, *flagWarmup, *flagIterations, *flagConcurrency)
	result.Operations[createRes.Operation] = createRes
	if len(volIDs) > 0 {
		deleteRes := runDeleteVolume(awsCli, volIDs, *flagWarmup, *flagConcurrency)
		result.Operations[deleteRes.Operation] = deleteRes
	} else {
		t.Log("CreateVolume produced no volumes to delete; skipping DeleteVolume phase")
	}

	t.Log("phase: DescribeVolumes (single + list)")
	describePoolSize := min(*flagIterations, 5)
	describePool := createDescribePool(t, awsCli, tags, az, *flagVolumeGiB, describePoolSize)
	if len(describePool) > 0 {
		single, list := runDescribeVolumes(awsCli, describePool, *flagWarmup, *flagIterations, *flagConcurrency)
		result.Operations[single.Operation] = single
		result.Operations[list.Operation] = list
	} else {
		t.Log("no describe-pool volumes available; skipping DescribeVolumes phase")
	}

	t.Log("phase: CreateSnapshot / DeleteSnapshot")
	snapCreate, snapDelete, snapErr := runSnapshotPhase(t, awsCli, tags, az, *flagVolumeGiB, *flagWarmup, *flagIterations, *flagConcurrency)
	if snapErr != nil {
		t.Errorf("snapshot phase: %v", snapErr)
	} else {
		result.Operations[snapCreate.Operation] = snapCreate
		result.Operations[snapDelete.Operation] = snapDelete
	}

	if *flagAttachDetach {
		t.Log("phase: AttachVolume / DetachVolume")
		attachRes, detachRes, err := runAttachDetachPhase(t, fix, awsCli, tags, az, *flagVolumeGiB, *flagWarmup, *flagIterations, *flagConcurrency)
		if err != nil {
			t.Errorf("attach/detach phase: %v", err)
		} else {
			result.Operations[attachRes.Operation] = attachRes
			result.Operations[detachRes.Operation] = detachRes
		}
	} else {
		t.Log("attach/detach phase skipped (-attach-detach=false)")
	}

	writeResult(t, env, result)
}

// createDescribePool creates n durable volumes for the DescribeVolumes
// phase, tolerant of partial failure (returns however many succeeded).
func createDescribePool(t *testing.T, c *harness.AWSClient, tags []*ec2.Tag, az string, sizeGiB int64, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := range n {
		out, err := c.EC2.CreateVolume(&ec2.CreateVolumeInput{
			AvailabilityZone: aws.String(az),
			Size:             aws.Int64(sizeGiB),
		})
		if err != nil {
			t.Logf("describe-pool volume %d/%d: %v", i+1, n, err)
			continue
		}
		id := aws.StringValue(out.VolumeId)
		harness.RegisterVolumeTeardown(t, c, id)
		tagResource(c, id, tags)
		if _, timedOut := pollVolumeState(c, id, ec2.VolumeStateAvailable, volumeSettleTimeout); timedOut {
			t.Logf("describe-pool volume %s never became available", id)
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// gitSHA prefers the runtime repo's HEAD (accurate when run from a checkout,
// the expected common case) and falls back to buildGitSHA.
func gitSHA() string {
	if out, err := exec.Command("git", "rev-parse", "HEAD").Output(); err == nil {
		if sha := strings.TrimSpace(string(out)); sha != "" {
			return sha
		}
	}
	return buildGitSHA
}

// writeResult marshals result to JSON and writes it to -out, or
// $ARTIFACT_DIR/ebsctl-bench-<timestamp>.json when -out is unset.
func writeResult(t *testing.T, env *harness.Env, result *RunResult) {
	t.Helper()
	path := *flagOut
	if path == "" {
		dir := env.ArtifactDir
		if dir == "" {
			dir = "."
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		path = filepath.Join(dir, fmt.Sprintf("ebsctl-bench-%s.json", result.Meta.Timestamp.Format("20060102T150405Z")))
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write result %s: %v", path, err)
	}
	t.Logf("wrote benchmark result to %s", path)
}
