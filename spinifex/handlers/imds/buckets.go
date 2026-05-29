package handlers_imds

import (
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/nats-io/nats.go"
)

const (
	// KVBucketIMDSVPCVeth is the canonical record that a VPC has IMDS plumbing
	// installed. Keyed by vpcID, the value carries the per-VPC veth/LSP spec the
	// BindManager replays to materialise host-side state on every chassis.
	KVBucketIMDSVPCVeth        = "spinifex-network-imds-vpc-veth"
	KVBucketIMDSVPCVethVersion = 1

	// KVBucketENIByVPCIP is a pure reverse index (vpcID/ip → eniID) so the IMDS
	// handler can resolve a request's datapath-attested source IP to an ENI in
	// one KV read instead of an O(N) scan of the ENI bucket.
	KVBucketENIByVPCIP        = "spinifex-network-eni-by-vpc-ip"
	KVBucketENIByVPCIPVersion = 1
)

// InitBuckets opens (or creates) both IMDS KV buckets and runs any pending
// migrations. The IMDS service needs both handles, so they are initialised
// together.
//
// History is fixed at 1: both buckets are write-once-per-key (a VPC's plumbing
// record and an ENI's IP binding are fixed for their lifetime, deleted on
// teardown), so retained revisions waste storage and lengthen tombstone
// lifetime with no recovery benefit.
func InitBuckets(js nats.JetStreamContext, replicas int) (vpcVeth, eniByIP nats.KeyValue, err error) {
	vpcVeth, err = initIMDSBucket(js, KVBucketIMDSVPCVeth, KVBucketIMDSVPCVethVersion, replicas)
	if err != nil {
		return nil, nil, err
	}

	eniByIP, err = initIMDSBucket(js, KVBucketENIByVPCIP, KVBucketENIByVPCIPVersion, replicas)
	if err != nil {
		return nil, nil, err
	}

	return vpcVeth, eniByIP, nil
}

// InitENIByIPBucket opens (or creates) just the eni-by-vpc-ip reverse-index
// bucket. The ENI controller (VPC service) is its writer and runs in the
// daemon, which may start before the IMDS service on a fresh cluster, so it
// needs an init path independent of InitBuckets. Idempotent — both callers
// converge on the same history-1 config.
func InitENIByIPBucket(js nats.JetStreamContext, replicas int) (nats.KeyValue, error) {
	return initIMDSBucket(js, KVBucketENIByVPCIP, KVBucketENIByVPCIPVersion, replicas)
}

// initIMDSBucket opens (or creates) a single IMDS KV bucket and runs any
// pending migrations.
func initIMDSBucket(js nats.JetStreamContext, bucket string, version, replicas int) (nats.KeyValue, error) {
	if replicas < 1 {
		replicas = 1
	}

	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:   bucket,
		History:  1,
		Replicas: replicas,
	})
	if err != nil {
		kv, err = js.KeyValue(bucket)
		if err != nil {
			return nil, fmt.Errorf("open %s bucket: %w", bucket, err)
		}
	}

	if err := migrate.DefaultRegistry.RunKV(bucket, kv, version); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", bucket, err)
	}

	return kv, nil
}
