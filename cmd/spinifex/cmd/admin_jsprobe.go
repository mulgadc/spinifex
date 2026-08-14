package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/cobra"

	"github.com/mulgadc/spinifex/spinifex/daemon"
)

// canaryKey is written and read back by the JetStream write probe. It lives in
// the same bucket the control plane depends on, under a reserved prefix no
// other consumer reads.
const canaryKey = "_watchdog.canary"

// errProbeInconclusive marks a failure that happened before the probe ever
// reached JetStream: config load, connect, or bucket open. The watchdog must
// treat this as "learned nothing", not as JetStream refusing a write — during
// recovery the bucket may simply not exist yet.
var errProbeInconclusive = errors.New("probe did not reach JetStream")

var nodeJSProbeCmd = &cobra.Command{
	Use:   "js-probe",
	Short: "Verify JetStream still accepts writes on this node",
	Long: `Round-trip a canary key through the cluster-state KV bucket and exit non-zero
if the write or the read-back fails.

This exists because JetStream can latch a write error per message block and keep
serving reads: nats-server's filestore records the failure on the block and
returns it on every later write, and nothing clears it short of a restart. The
HTTP health endpoint reports the server-wide JetStream state, which that latch
never touches, so only an actual write observes it.`,
	Run: runNodeJSProbe,
}

func runNodeJSProbe(cmd *cobra.Command, _ []string) {
	timeout, _ := cmd.Flags().GetDuration("timeout")
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	if err := probeJetStreamWrite(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "js-probe: %v\n", err)
		if errors.Is(err, errProbeInconclusive) {
			os.Exit(2)
		}
		os.Exit(1)
	}
	fmt.Println("js-probe: ok")
}

// probeJetStreamWrite writes a canary value and reads it back, returning the
// first error. It deliberately probes the existing cluster-state bucket rather
// than a bucket of its own: a dedicated bucket is a separate stream that can be
// placed on a different node, so it could stay writable while the bucket the
// control plane actually depends on is wedged.
//
// Every stage before canaryRoundTrip is wrapped in errProbeInconclusive: the
// probe has observed nothing about JetStream's write path yet, so a caller
// must not read config, connect, or bucket-open failures as a JetStream
// failure.
func probeJetStreamWrite(ctx context.Context) error {
	_, nc, err := loadConfigAndConnect()
	if err != nil {
		return fmt.Errorf("%w: connect: %w", errProbeInconclusive, err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("%w: jetstream: %w", errProbeInconclusive, err)
	}

	kv, err := js.KeyValue(ctx, daemon.ClusterStateBucket)
	if err != nil {
		return fmt.Errorf("%w: open bucket %s: %w", errProbeInconclusive, daemon.ClusterStateBucket, err)
	}
	return canaryRoundTrip(ctx, kv)
}

// canaryRoundTrip writes a unique value and reads it back, returning the first
// error. The write is what matters: a latched block returns its stored error on
// every subsequent write, so this is the only operation that observes it.
func canaryRoundTrip(ctx context.Context, kv jetstream.KeyValue) error {
	want := []byte(time.Now().UTC().Format(time.RFC3339Nano))
	if _, err := kv.Put(ctx, canaryKey, want); err != nil {
		return fmt.Errorf("write canary: %w", err)
	}

	entry, err := kv.Get(ctx, canaryKey)
	if err != nil {
		return fmt.Errorf("read back canary: %w", err)
	}

	// A stale value means the write was accepted but not durable, which is the
	// same operational problem as an outright write failure.
	if string(entry.Value()) != string(want) {
		return fmt.Errorf("canary read back stale: wrote %q, read %q", want, entry.Value())
	}
	return nil
}
