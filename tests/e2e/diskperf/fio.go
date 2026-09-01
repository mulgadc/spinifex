//go:build e2e

package diskperf

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// Guest-side exit codes, distinct from fio's own so an infra fault is never
// reported as a performance result. The suite is only meaningful when the tool
// under measurement actually ran.
const (
	exitAptUpdate  = 10
	exitAptInstall = 11
)

// p999Key is fio's percentile map key for p99.9. fio emits percentiles as
// fixed-format decimal strings, so this is a literal rather than a formatted
// float.
const p999Key = "99.900000"

// jobSpec is one pinned fio profile. Every field is fixed in code rather than
// read from the environment: a gate whose workload can drift is comparing
// against a baseline that no longer describes it.
type jobSpec struct {
	// Name is both the fio job name and the baseline key, so renaming one
	// without the other retires the baseline entry rather than silently
	// comparing against the wrong workload.
	Name string
	// RW is an fio --rw value. MixRead applies to randrw only.
	RW      string
	MixRead int

	BlockSize string
	// SizeGiB is per job. NumJobs jobs each take their own SizeGiB slice of the
	// device via offset_increment, so the working set is NumJobs*SizeGiB.
	SizeGiB int
	NumJobs int
	IODepth int

	// Budget bounds the guest-side invocation. Exceeding it is an availability
	// failure, not a slow result: the workload did not complete.
	Budget time.Duration

	// Availability marks the job whose run the responsiveness probes bracket.
	// Sustained random write is the pattern that produced the original stall.
	Availability bool
}

// gateJobs is the pinned profile set. randwrite-4k is the job that took the
// guest and the host down, at the 4x4 GiB shape the exit criterion names;
// randrw-70-30-4k is the mixed workload the headline IOPS figure came from.
var gateJobs = []jobSpec{
	{
		Name:         "randwrite-4k",
		RW:           "randwrite",
		BlockSize:    "4k",
		SizeGiB:      4,
		NumJobs:      4,
		IODepth:      32,
		Budget:       45 * time.Minute,
		Availability: true,
	},
	{
		Name:      "randrw-70-30-4k",
		RW:        "randrw",
		MixRead:   70,
		BlockSize: "4k",
		SizeGiB:   4,
		NumJobs:   4,
		IODepth:   32,
		Budget:    45 * time.Minute,
	},
}

// workingSetGiB is the span of the device the job touches, and so the minimum
// volume size it needs.
func (j jobSpec) workingSetGiB() int { return j.SizeGiB * j.NumJobs }

// command renders the fio invocation. The target is a raw device with
// direct=1, so the guest page cache and any filesystem are out of the path;
// offset_increment gives each job a disjoint region rather than four jobs
// overwriting the same one.
func (j jobSpec) command(dev, outPath string) string {
	args := []string{
		"sudo fio",
		"--name=" + j.Name,
		"--filename=/dev/" + dev,
		"--rw=" + j.RW,
		"--bs=" + j.BlockSize,
		fmt.Sprintf("--size=%dG", j.SizeGiB),
		fmt.Sprintf("--offset_increment=%dG", j.SizeGiB),
		fmt.Sprintf("--numjobs=%d", j.NumJobs),
		fmt.Sprintf("--iodepth=%d", j.IODepth),
		"--ioengine=libaio",
		"--direct=1",
		"--group_reporting",
		"--output-format=json",
		"--output=" + outPath,
	}
	if j.RW == "randrw" {
		args = append(args, fmt.Sprintf("--rwmixread=%d", j.MixRead))
	}
	return strings.Join(args, " ")
}

// fioReport is the subset of fio's JSON output the gate reads.
type fioReport struct {
	Version string   `json:"fio version"`
	Jobs    []fioJob `json:"jobs"`
}

type fioJob struct {
	Name  string    `json:"jobname"`
	Read  fioStream `json:"read"`
	Write fioStream `json:"write"`
}

type fioStream struct {
	IOBytes int64   `json:"io_bytes"`
	BWBytes int64   `json:"bw_bytes"`
	IOPS    float64 `json:"iops"`
	Clat    fioClat `json:"clat_ns"`
}

type fioClat struct {
	Max        int64            `json:"max"`
	Mean       float64          `json:"mean"`
	Percentile map[string]int64 `json:"percentile"`
}

// p999Ms returns the p99.9 completion latency in milliseconds, or 0 when fio
// did not emit the percentile because the stream moved no I/O.
func (s fioStream) p999Ms() float64 {
	return float64(s.Clat.Percentile[p999Key]) / 1e6
}

// maxMs returns the worst completion latency in milliseconds.
func (s fioStream) maxMs() float64 { return float64(s.Clat.Max) / 1e6 }

// runResult is one execution of one job.
type runResult struct {
	Job    string
	Report fioReport
	// Aggregate is the group_reporting job entry. fio emits one per --name
	// under group_reporting, so this is the whole run for that job.
	Aggregate fioJob
}

// installFio makes fio available in the guest. Distinct exit codes keep an apt
// failure -- a network or mirror fault, not a storage one -- out of the
// performance verdict.
func installFio(tgt harness.SSHTarget) error {
	script := strings.Join([]string{
		"set -e",
		"if command -v fio >/dev/null 2>&1; then exit 0; fi",
		fmt.Sprintf("sudo apt-get update -y || exit %d", exitAptUpdate),
		fmt.Sprintf("sudo DEBIAN_FRONTEND=noninteractive apt-get install -y fio || exit %d", exitAptInstall),
	}, "\n")
	if out, err := harness.GuestExecTimeout(tgt, script, 10*time.Minute); err != nil {
		return fmt.Errorf("installing fio in the guest failed — this is an infra fault, not a performance result: %w\n%s", err, out)
	}
	return nil
}

// runFio drives one job to completion and returns its parsed report. The JSON
// is written to a file in the guest and fetched by a second command: fio's
// forced-exit path on a wedged job corrupts its own heap and produces no
// output, so a run that exceeds its budget must still be distinguishable from
// one that failed to parse.
func runFio(tgt harness.SSHTarget, j jobSpec, dev string) (runResult, error) {
	outPath := "/tmp/diskperf-" + j.Name + ".json"
	if out, err := harness.GuestExec(tgt, "sudo rm -f "+outPath); err != nil {
		return runResult{}, fmt.Errorf("clearing %s: %w\n%s", outPath, err, out)
	}

	if out, err := harness.GuestExecTimeout(tgt, j.command(dev, outPath), j.Budget); err != nil {
		return runResult{}, fmt.Errorf("fio job %q did not complete within %s — the workload stalled: %w\n%s", j.Name, j.Budget, err, out)
	}

	raw, err := harness.GuestExec(tgt, "sudo cat "+outPath)
	if err != nil {
		return runResult{}, fmt.Errorf("reading %s: %w\n%s", outPath, err, raw)
	}
	return parseFio(j.Name, raw)
}

// parseFio extracts the named job's aggregate from fio's JSON. fio prefixes
// its output with progress lines on some builds, so the object is located by
// its opening brace rather than assumed to start at byte zero.
func parseFio(name, raw string) (runResult, error) {
	start := strings.Index(raw, "{")
	if start < 0 {
		return runResult{}, fmt.Errorf("fio produced no JSON object for %q:\n%s", name, raw)
	}
	var rep fioReport
	if err := json.Unmarshal([]byte(raw[start:]), &rep); err != nil {
		return runResult{}, fmt.Errorf("parsing fio JSON for %q: %w\n%s", name, err, raw)
	}
	for _, jb := range rep.Jobs {
		if jb.Name != name {
			continue
		}
		if jb.Read.IOBytes == 0 && jb.Write.IOBytes == 0 {
			return runResult{}, fmt.Errorf("fio job %q moved no bytes — it ran but did nothing, so its numbers describe nothing", name)
		}
		return runResult{Job: name, Report: rep, Aggregate: jb}, nil
	}
	return runResult{}, fmt.Errorf("fio JSON has no job named %q (found %s)", name, strings.Join(jobNames(rep), ", "))
}

func jobNames(rep fioReport) []string {
	names := make([]string, 0, len(rep.Jobs))
	for _, j := range rep.Jobs {
		names = append(names, j.Name)
	}
	return names
}

// median returns the middle value of xs, averaging the two central values for
// an even count. xs is copied so the caller's ordering survives.
func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}
