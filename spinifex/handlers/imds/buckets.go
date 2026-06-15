package handlers_imds

import (
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/nats-io/nats.go"
)

const (
	// KVBucketIMDSSubnetVeth records per-subnet IMDS localport state replayed on each chassis.
	KVBucketIMDSSubnetVeth        = "spinifex-network-imds-subnet-veth"
	KVBucketIMDSSubnetVethVersion = 1

	// KVBucketENIByVPCIP is a reverse index (vpcID/ip → eniID) for O(1) source-IP resolution.
	KVBucketENIByVPCIP        = "spinifex-network-eni-by-vpc-ip"
	KVBucketENIByVPCIPVersion = 1
)

// InitBuckets opens or creates both IMDS KV buckets and runs pending migrations.
func InitBuckets(js nats.JetStreamContext, replicas int) (subnetVeth, eniByIP nats.KeyValue, err error) {
	subnetVeth, err = initIMDSBucket(js, KVBucketIMDSSubnetVeth, KVBucketIMDSSubnetVethVersion, replicas)
	if err != nil {
		return nil, nil, err
	}

	eniByIP, err = initIMDSBucket(js, KVBucketENIByVPCIP, KVBucketENIByVPCIPVersion, replicas)
	if err != nil {
		return nil, nil, err
	}

	return subnetVeth, eniByIP, nil
}

// InitENIByIPBucket opens or creates the eni-by-vpc-ip bucket independently of InitBuckets.
func InitENIByIPBucket(js nats.JetStreamContext, replicas int) (nats.KeyValue, error) {
	return initIMDSBucket(js, KVBucketENIByVPCIP, KVBucketENIByVPCIPVersion, replicas)
}

// initIMDSBucket opens or creates a single IMDS KV bucket and runs pending migrations.
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
