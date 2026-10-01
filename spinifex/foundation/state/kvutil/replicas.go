package kvutil

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nats.go/jetstream"
)

// streamPrefix is how JetStream names the stream behind a KV bucket. Raising a
// bucket's replica count is a stream update, so the name has to be built here.
const streamPrefix = "KV_"

// BucketReport is one bucket's replication, as the cluster currently holds it.
//
// Configured replicas and working replicas are separate fields because they are
// separate facts. A stream can report the count it was asked for while a peer is
// offline or still catching up, and that reads as healthy right up until the
// node actually serving it goes away.
type BucketReport struct {
	Bucket   string `json:"bucket"`
	Replicas int    `json:"replicas"`
	Want     int    `json:"want"`
	Cluster  string `json:"cluster,omitempty"`

	// Leader is the server currently accepting writes, empty when there is none.
	Leader string `json:"leader,omitempty"`

	// Online counts the peers that are caught up and reachable, the leader
	// included. This is the number a majority is measured against.
	Online int `json:"online"`

	// Peers names every server assigned to the bucket, leader first.
	Peers []string `json:"peers,omitempty"`
}

// UnderReplicated reports whether this bucket has fewer replicas than the
// cluster can give it. A bucket at the cluster's replica count is correct; one
// below it is a cluster-wide service living on fewer nodes than exist.
func (r BucketReport) UnderReplicated() bool { return r.Replicas < r.Want }

// HasQuorum reports whether enough of the bucket's replicas are working for it
// to accept a write.
//
// Measured against the configured count rather than the wanted one: a bucket
// that has not been raised yet is still perfectly able to serve at the size it
// has, and calling that a quorum failure would confuse a durability shortfall
// with an outage.
func (r BucketReport) HasQuorum() bool { return r.Online > r.Replicas/2 }

// Healthy reports whether the bucket is both at the cluster's replica count and
// able to serve.
func (r BucketReport) Healthy() bool { return !r.UnderReplicated() && r.HasQuorum() }

// AuditBucketReplicas reports every KV bucket's replica count against what the
// cluster's size entitles it to. It reads and changes nothing, so it is safe to
// run against a live cluster and is what both the admin command and the e2e
// assertion are built on.
func AuditBucketReplicas(ctx context.Context, js jetstream.JetStream) ([]BucketReport, error) {
	want, err := clustersize.Replicas()
	if err != nil {
		return nil, err
	}

	names, err := BucketNames(ctx, js)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)

	reports := make([]BucketReport, 0, len(names))
	for _, name := range names {
		info, err := js.Stream(ctx, streamPrefix+name)
		if err != nil {
			// A bucket deleted between the listing and the read is not a finding.
			if errors.Is(err, jetstream.ErrStreamNotFound) {
				continue
			}
			return nil, fmt.Errorf("read KV bucket %s: %w", name, err)
		}
		cached, err := info.Info(ctx)
		if err != nil {
			return nil, fmt.Errorf("read KV bucket %s: %w", name, err)
		}
		reports = append(reports, newBucketReport(name, want, cached))
	}
	return reports, nil
}

// newBucketReport reads one stream's replication and placement out of its info.
//
// A non-clustered server reports no cluster block at all. Its single replica is
// nonetheless online, because the read that produced this info was answered, and
// reporting zero there would make every single-node bucket look like it had lost
// quorum.
func newBucketReport(bucket string, want int, info *jetstream.StreamInfo) BucketReport {
	report := BucketReport{Bucket: bucket, Replicas: info.Config.Replicas, Want: want}
	if info.Cluster == nil {
		report.Online = 1
		return report
	}

	report.Cluster = info.Cluster.Name
	report.Leader = info.Cluster.Leader
	if report.Leader != "" {
		report.Peers = append(report.Peers, report.Leader)
		report.Online++
	}
	for _, peer := range info.Cluster.Replicas {
		report.Peers = append(report.Peers, peer.Name)
		// Current and not offline is JetStream's own statement that this peer is
		// caught up and reachable, which is what a majority has to be counted
		// from. A peer that is merely assigned cannot acknowledge a write.
		if peer.Current && !peer.Offline {
			report.Online++
		}
	}
	return report
}

// RaiseAllBucketReplicas raises every KV bucket to the cluster's replica count
// and reports how many it had to change.
//
// It is the cluster-wide counterpart to the raise a bucket gets when a service
// opens it: a node joining changes the answer for every bucket at once, and
// most of them will not be opened again until something restarts.
//
// Every bucket is attempted even after one fails, and the failures come back
// joined. Stopping at the first would leave the rest of a cluster's buckets
// short because one of them could not be placed, which is the opposite of what
// a sweep is for.
func RaiseAllBucketReplicas(ctx context.Context, js jetstream.JetStream) (int, error) {
	reports, err := AuditBucketReplicas(ctx, js)
	if err != nil {
		return 0, err
	}
	raised := 0
	var failures []error
	for _, r := range reports {
		if !r.UnderReplicated() {
			continue
		}
		if err := RaiseBucketReplicas(ctx, js, r.Bucket, r.Want); err != nil {
			failures = append(failures, err)
			continue
		}
		raised++
	}
	return raised, errors.Join(failures...)
}

// raiseRetryEvery is how often the open path will re-attempt a raise for the
// same bucket. It is a floor on retries, not a deadline: the next open after the
// window tries again, so a cluster that gains a node still converges without an
// operator.
const raiseRetryEvery = 1 * time.Minute

// openRaises throttles the raise attempts made on the open path.
var openRaises = newRaiseThrottle(raiseRetryEvery)

// TryRaiseBucketReplicas raises a bucket toward want on the path a service takes
// when it opens one, warning rather than failing when it cannot.
//
// A failed raise is not a failed open. The config can legitimately name nodes
// that are not serving yet — that is what growing a cluster looks like — and
// refusing would stop every service on the host over a bucket that is present
// and quorate at the count it has.
//
// Throttled per bucket, because callers reopen buckets on every reconcile tick.
// Without it a cluster that cannot place the replica bills a stream update to the
// meta leader and writes a warning line every few seconds, for every bucket, on
// every node — load and noise arriving exactly when a cluster is already unwell.
func TryRaiseBucketReplicas(ctx context.Context, js jetstream.KeyValueManager, bucket string, want int) {
	if !openRaises.allow(bucket, time.Now()) {
		return
	}
	if err := RaiseBucketReplicas(ctx, js, bucket, want); err != nil {
		slog.WarnContext(ctx, "Could not raise KV bucket to the cluster's replica count; it stays where it is until the cluster can hold more",
			"bucket", bucket, "want", want, "error", err)
	}
}

// raiseThrottle records when each bucket was last attempted, so a caller on a
// reconcile loop retries on a schedule of its own rather than the loop's.
type raiseThrottle struct {
	mu    sync.Mutex
	every time.Duration
	last  map[string]time.Time
}

func newRaiseThrottle(every time.Duration) *raiseThrottle {
	return &raiseThrottle{every: every, last: make(map[string]time.Time)}
}

// allow reports whether bucket may be attempted at now, recording the attempt
// when it may. The first attempt for a bucket is always allowed, so a raise is
// never delayed by the throttle existing.
func (t *raiseThrottle) allow(bucket string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, seen := t.last[bucket]; seen && now.Sub(last) < t.every {
		return false
	}
	t.last[bucket] = now
	return true
}

// RaiseBucketReplicas raises a bucket to want replicas, leaving every other
// setting as it is.
//
// It updates the stream rather than the bucket deliberately. A KV update takes
// a KeyValueConfig, which cannot express a stream's full configuration, so
// applying one would quietly reset whatever the caller's literal omitted —
// history and TTL among them. Reading the live config and changing one field
// cannot do that.
//
// Lowering is never done: a bucket with more replicas than the cluster is
// entitled to is not a fault, and a cluster that has lost nodes must not have
// its durability reduced to match.
func RaiseBucketReplicas(ctx context.Context, js jetstream.KeyValueManager, bucket string, want int) error {
	streams, ok := js.(jetstream.StreamManager)
	if !ok {
		return fmt.Errorf("raise KV bucket %s to %d replicas: no stream manager available", bucket, want)
	}

	stream, err := streams.Stream(ctx, streamPrefix+bucket)
	if err != nil {
		return fmt.Errorf("read KV bucket %s: %w", bucket, err)
	}
	cfg := stream.CachedInfo().Config
	if cfg.Replicas >= want {
		return nil
	}

	was := cfg.Replicas
	cfg.Replicas = want
	if _, err := streams.UpdateStream(ctx, cfg); err != nil {
		return fmt.Errorf("raise KV bucket %s from %d to %d replicas: %w", bucket, was, want, err)
	}
	slog.InfoContext(ctx, "raised a KV bucket to the cluster's replica count",
		"bucket", bucket, "was", was, "now", want)
	return nil
}
