package kvutil

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/mulgadc/spinifex/spinifex/clustersize"
	"github.com/nats-io/nats.go/jetstream"
)

// streamPrefix is how JetStream names the stream behind a KV bucket. Raising a
// bucket's replica count is a stream update, so the name has to be built here.
const streamPrefix = "KV_"

// BucketReport is one bucket's replication, as the cluster currently holds it.
type BucketReport struct {
	Bucket   string   `json:"bucket"`
	Replicas int      `json:"replicas"`
	Want     int      `json:"want"`
	Cluster  string   `json:"cluster,omitempty"`
	Peers    []string `json:"peers,omitempty"`
}

// UnderReplicated reports whether this bucket has fewer replicas than the
// cluster can give it. A bucket at the cluster's node count is correct; one
// below it is a cluster-wide service living on fewer nodes than exist.
func (r BucketReport) UnderReplicated() bool { return r.Replicas < r.Want }

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
		report := BucketReport{Bucket: name, Replicas: cached.Config.Replicas, Want: want}
		if cached.Cluster != nil {
			report.Cluster = cached.Cluster.Name
			if cached.Cluster.Leader != "" {
				report.Peers = append(report.Peers, cached.Cluster.Leader)
			}
			for _, peer := range cached.Cluster.Replicas {
				report.Peers = append(report.Peers, peer.Name)
			}
		}
		reports = append(reports, report)
	}
	return reports, nil
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
