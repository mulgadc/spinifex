package projection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	ec2vpcendpoint "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpcendpoint"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// EndpointBuckets are the KV buckets whose changes alter which subnets route
// to a gateway endpoint.
var EndpointBuckets = []string{
	ec2vpcendpoint.KVBucketVpcEndpoints,
	ec2routetable.KVBucketRouteTables,
	ec2vpc.KVBucketSubnets,
}

// GatewayEndpoint is the endpoint a subnet's route table routes S3 to.
type GatewayEndpoint struct {
	ID          string
	AccountID   string
	VpcID       string
	ServiceName string
	Generation  int64
}

// LoadGatewayEndpoints maps each subnet whose route table holds an effective
// gateway endpoint route to that endpoint. A missing bucket holds nothing;
// any other read failure is an error, never a partial map.
func LoadGatewayEndpoints(ctx context.Context, js jetstream.JetStream) (map[string]GatewayEndpoint, error) {
	kv, err := openBucket(ctx, js, ec2vpcendpoint.KVBucketVpcEndpoints)
	if kv == nil || err != nil {
		return nil, err
	}
	assocs, err := ec2vpcendpoint.ListAssociations(ctx, kv)
	if err != nil || len(assocs) == 0 {
		return nil, err
	}
	tables, err := loadRecords[ec2routetable.RouteTableRecord](ctx, js, ec2routetable.KVBucketRouteTables)
	if err != nil {
		return nil, err
	}
	subnets, err := loadRecords[ec2vpc.SubnetRecord](ctx, js, ec2vpc.KVBucketSubnets)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*ec2routetable.RouteTableRecord, len(tables))
	for i := range tables {
		byID[tables[i].RouteTableId] = &tables[i]
	}
	governed := tableSubnets(tables, subnets)

	out := map[string]GatewayEndpoint{}
	for _, a := range assocs {
		rt := byID[a.RouteTableID]
		if rt == nil || rt.VpcId != a.Endpoint.VpcID {
			continue
		}
		e := GatewayEndpoint{
			ID: a.Endpoint.ID, AccountID: a.Endpoint.AccountID, VpcID: a.Endpoint.VpcID,
			ServiceName: a.Endpoint.ServiceName, Generation: a.Endpoint.Generation,
		}
		for _, subnet := range governed[rt.RouteTableId] {
			out[subnet] = e
		}
	}
	return out, nil
}

// tableSubnets maps each table to the subnets it governs: its explicit
// associations, and for a main table every subnet of its VPC with none.
func tableSubnets(tables []ec2routetable.RouteTableRecord, subnets []ec2vpc.SubnetRecord) map[string][]string {
	explicit := map[string]bool{}
	out := make(map[string][]string, len(tables))
	for _, rt := range tables {
		for _, a := range rt.Associations {
			if a.SubnetId != "" && !a.Main {
				explicit[a.SubnetId] = true
				out[rt.RouteTableId] = append(out[rt.RouteTableId], a.SubnetId)
			}
		}
	}
	for _, rt := range tables {
		if !rt.IsMain {
			continue
		}
		for _, sn := range subnets {
			if sn.VpcId == rt.VpcId && !explicit[sn.SubnetId] {
				out[rt.RouteTableId] = append(out[rt.RouteTableId], sn.SubnetId)
			}
		}
	}
	return out
}

func openBucket(ctx context.Context, js jetstream.JetStream, bucket string) (jetstream.KeyValue, error) {
	kv, err := js.KeyValue(ctx, bucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", bucket, err)
	}
	return kv, nil
}

// loadRecords decodes every record in bucket; a key deleted between the list
// and the read is skipped.
func loadRecords[T any](ctx context.Context, js jetstream.JetStream, bucket string) ([]T, error) {
	kv, err := openBucket(ctx, js, bucket)
	if kv == nil || err != nil {
		return nil, err
	}
	keys, err := kvstore.Keys(ctx, kv)
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", bucket, err)
	}
	var out []T
	for _, key := range keys {
		if key == kvstore.VersionKey {
			continue
		}
		entry, err := kv.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s/%s: %w", bucket, key, err)
		}
		var v T
		if err := json.Unmarshal(entry.Value(), &v); err != nil {
			return nil, fmt.Errorf("decode %s/%s: %w", bucket, key, err)
		}
		out = append(out, v)
	}
	return out, nil
}
