package conformance

// The rest of this package prices the control verbs: create, publish,
// describe. Guest I/O never crosses those, so none of those numbers say
// anything about what a guest actually experiences. This file drives the
// published export with qemu-img bench, which is the same block layer a
// guest's virtio-blk sits on.

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/stretchr/testify/require"
)

// dataPathVolumeBytes is large enough that a strided profile can skip far
// enough to defeat readahead, and small enough to stay quick over a real
// store.
const dataPathVolumeBytes int64 = 256 << 20

// DataPathProfile is one qemu-img bench run: a request size, a queue depth
// and how far apart consecutive requests land.
type DataPathProfile struct {
	Name string

	// BufferSize is the size of each request.
	BufferSize int64

	// StepSize is the offset increment between requests. Zero means
	// sequential, i.e. one buffer.
	StepSize int64

	Count int
	Depth int
	Write bool

	// FlushInterval, when set, flushes every N write requests. Without it a
	// write profile measures how fast the WAL accepts data, not how fast the
	// store takes it: chunk upload is asynchronous.
	FlushInterval int
}

func (p DataPathProfile) step() int64 {
	if p.StepSize == 0 {
		return p.BufferSize
	}
	return p.StepSize
}

// bytes is how much payload the profile moves, which is not the span it
// covers when it strides.
func (p DataPathProfile) bytes() int64 {
	return p.BufferSize * int64(p.Count)
}

// DataPathResult is one profile's timing as qemu-img reported it. The
// harness's own wall clock is deliberately not used: it would fold in
// process startup and the NBD handshake, which a running guest pays once
// rather than per request.
type DataPathResult struct {
	Profile DataPathProfile
	Elapsed time.Duration
}

// ThroughputMiBs is the payload rate.
func (r DataPathResult) ThroughputMiBs() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(r.Profile.bytes()) / (1 << 20) / r.Elapsed.Seconds()
}

// PerRequest is elapsed divided by request count. At depth > 1 this is
// throughput expressed per request, not the latency of one request.
func (r DataPathResult) PerRequest() time.Duration {
	if r.Profile.Count == 0 {
		return 0
	}
	return r.Elapsed / time.Duration(r.Profile.Count)
}

// writtenSpan is how much of the volume the leading write profile covers.
// Every read profile stays inside it: a read past the written region is
// answered as a hole, which measures nothing.
const writtenSpan int64 = 64 << 20

// DefaultDataPathProfiles writes before it reads, so the reads have
// something to find. A read profile that ran first would be measuring how
// fast the export can answer with zeroes.
func DefaultDataPathProfiles() []DataPathProfile {
	return []DataPathProfile{
		{Name: "seq-write-64k", BufferSize: 64 << 10, Count: 1024, Depth: 16, Write: true},
		{Name: "seq-write-4k", BufferSize: 4 << 10, Count: 4096, Depth: 16, Write: true},
		{Name: "seq-write-64k-flush", BufferSize: 64 << 10, Count: 256, Depth: 16, Write: true, FlushInterval: 16},
		{Name: "seq-read-64k", BufferSize: 64 << 10, Count: 1024, Depth: 16},
		{Name: "seq-read-4k", BufferSize: 4 << 10, Count: 4096, Depth: 16},
		// 256 requests one 256K step apart covers exactly the written span.
		{Name: "strided-read-4k", BufferSize: 4 << 10, StepSize: writtenSpan / 256, Count: 256, Depth: 16},
		{Name: "seq-read-64k-qd1", BufferSize: 64 << 10, Count: 256, Depth: 1},
	}
}

// dataPathSettle is waited out between profiles. Each profile is its own NBD
// connection, and the viperblock nbdkit plugin tears an engine down per
// disconnect, removing the volume's local directory as it goes. A connection
// opened while the previous one is still closing fails the handshake.
const dataPathSettle = 2 * time.Second

// RequireDataPathTools skips when qemu-img is absent rather than reporting a
// data path nothing measured.
func RequireDataPathTools(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed (qemu-utils)")
	}
}

// benchElapsed pulls the timing out of qemu-img bench's own report.
var benchElapsed = regexp.MustCompile(`Run completed in ([0-9.]+) seconds`)

// runDataPathProfile executes one profile against uri.
func runDataPathProfile(t *testing.T, uri string, p DataPathProfile) DataPathResult {
	t.Helper()

	args := []string{
		"bench", "-f", "raw",
		"-c", strconv.Itoa(p.Count),
		"-s", strconv.FormatInt(p.BufferSize, 10),
		"-S", strconv.FormatInt(p.step(), 10),
		"-d", strconv.Itoa(p.Depth),
	}
	if p.Write {
		args = append(args, "-w")
		if p.FlushInterval > 0 {
			args = append(args, "--flush-interval", strconv.Itoa(p.FlushInterval))
		}
	}
	args = append(args, uri)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "qemu-img", args...).CombinedOutput()
	require.NoErrorf(t, err, "qemu-img %v: %s", args, out)

	match := benchElapsed.FindSubmatch(out)
	require.NotNilf(t, match, "qemu-img bench reported no elapsed time: %s", out)
	seconds, err := strconv.ParseFloat(string(match[1]), 64)
	require.NoError(t, err)

	return DataPathResult{Profile: p, Elapsed: time.Duration(seconds * float64(time.Second))}
}

// RunDataPathSuite publishes one volume and drives every profile against it,
// logging a table. It asserts only that each run completes and moves a
// non-zero amount of data: what a given store is capable of is not a
// contract, so pinning a number here would only produce a flaky test.
func RunDataPathSuite(t *testing.T, newProvider func(t *testing.T) ebsprovider.EBSProvider, cfg NBDClientConfig) {
	RequireNBDTools(t)
	RequireDataPathTools(t)

	cfg = cfg.withDefaults()
	if cfg.VolumeBytes < dataPathVolumeBytes {
		cfg.VolumeBytes = dataPathVolumeBytes
	}

	provider := newProvider(t)
	volumeID := cfg.VolumePrefix + "datapath"

	pub := publishForNBD(t, provider, cfg, volumeID, false)
	assertNBDURI(t, pub.NBDURI)
	uri := dialURI(t, pub.NBDURI)

	export := nbdInfo(t, uri)
	require.Equalf(t, cfg.VolumeBytes, export.Size, "export size does not match the volume that was created")

	results := make([]DataPathResult, 0, len(DefaultDataPathProfiles()))
	for _, profile := range DefaultDataPathProfiles() {
		time.Sleep(dataPathSettle)
		result := runDataPathProfile(t, uri, profile)
		require.Positivef(t, result.Elapsed, "%s completed in no measurable time", profile.Name)
		results = append(results, result)
	}

	t.Log(formatDataPathResults(results))
}

// formatDataPathResults renders the table that is the point of the run.
func formatDataPathResults(results []DataPathResult) string {
	report := fmt.Sprintf("\n%-18s %8s %6s %6s %10s %12s %12s\n",
		"profile", "req", "size", "depth", "elapsed", "MiB/s", "per-req")
	for _, r := range results {
		report += fmt.Sprintf("%-18s %8d %5dK %6d %10s %12.1f %12s\n",
			r.Profile.Name, r.Profile.Count, r.Profile.BufferSize>>10, r.Profile.Depth,
			r.Elapsed.Round(time.Millisecond), r.ThroughputMiBs(), r.PerRequest().Round(time.Microsecond))
	}
	return report
}
