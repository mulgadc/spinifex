//go:build e2e && bench

package ebsctl

import "flag"

// buildGitSHA is overridable at build time via
// -ldflags "-X 'github.com/mulgadc/spinifex/tests/bench/ebsctl.buildGitSHA=<sha>'".
// gitSHA() (main_test.go) prefers a runtime `git rev-parse HEAD` (works when
// run from a repo checkout, the common case) and falls back to this when
// that fails.
var buildGitSHA = "unknown"

// Flags are declared in a regular (non-_test.go) file, not main_test.go,
// so that provider.go and other non-test files in this package can reference
// them directly: a non-test file may not reference a symbol declared only in
// a _test.go file (Go excludes _test.go from a plain `go build`/`go vet` of
// the package, only including it when building the test binary via
// `go test`).
var (
	flagOut          = flag.String("out", "", "output JSON path (default: $ARTIFACT_DIR/ebsctl-bench-<timestamp>.json)")
	flagIterations   = flag.Int("iterations", 50, "measured iterations per operation, per worker")
	flagWarmup       = flag.Int("warmup", 5, "discarded warm-up iterations per operation, per worker")
	flagConcurrency  = flag.Int("concurrency", 1, "concurrent workers per operation")
	flagProvider     = flag.String("provider", "", "expected/fallback [ebs] provider (viperblockd); cross-checked against cluster detection when detection succeeds, required when it doesn't")
	flagAttachDetach = flag.Bool("attach-detach", true, "include the attach/detach phase (boots and terminates one instance)")
	flagVolumeGiB    = flag.Int64("volume-size-gib", 1, "size in GiB of every volume this benchmark creates")
)
