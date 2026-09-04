package gateway_ec2_instance

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/instancecache"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// describeCacheOpName labels the log lines ParseInstanceListSelection emits
// for the cache-served path, distinguishing them from the KV path's own
// per-bucket opName.
const describeCacheOpName = "DescribeInstances(cache)"

// describeFallbackStateCode/Name label an instance whose stored status has no
// vm.EC2APIState mapping. A cache-served listing spans every non-terminated
// lifecycle state rather than one bucket, so it takes the same fallback the
// per-node running path uses: an unmapped status is presumed still alive
// rather than presumed stopped.
const (
	describeFallbackStateCode = 0
	describeFallbackStateName = "pending"
)

// CacheReader is what a cache-served DescribeInstances needs from
// instancecache.Cache: the account's whole set, a per-ID lookup for a request
// that names its instances, and whether the read happened while the cache was
// degraded. Distinct from recordLister (status_synthesis.go), which the
// status-synthesis backfill uses and only needs List — widening that
// interface would hand status synthesis methods it has no business calling.
type CacheReader interface {
	List(ctx context.Context, accountID string) ([]*vm.VM, bool)
	Get(ctx context.Context, instanceID string) (*vm.VM, error)
	Degraded() bool
}

var _ CacheReader = (*instancecache.Cache)(nil)

// includeInCacheDescribe reports whether v belongs in a cache-served
// DescribeInstances answer, matching what the union of the per-node fan-out
// and the stopped KV bucket would return today.
//
// StateTerminated is excluded outright: the unified record space keeps a
// terminated record around indefinitely when its owning node never came back
// to retire it (see retireRecord/runsOn), but neither the fan-out nor the
// terminated bucket — a separate KV store this cache does not watch — would
// ever surface that record, so including it here would be a new leak.
//
// A StateStopped record is included only when vm.OperatorStopped is true.
// An operator-stopped record is exactly what the stopped KV bucket's own
// operatorStopped predicate returns. A drain-stopped record (StateStopped,
// DesiredRunning) is neither: it sits in the record space with no owning
// node's vmMgr reporting it (that node stopped the process as part of its own
// drain) and the stopped bucket predicate excludes it too, so today it is
// invisible from every source this cache-served path stands in for.
//
// Every other state — provisioning, pending, running, stopping,
// shutting-down, error — is included unconditionally: the per-node responder
// (InstanceServiceImpl.DescribeInstances) projects whatever its vmMgr is
// currently holding with no state-based exclusion beyond visibility, ID, and
// filters, so a cache-served listing must not invent one either.
func includeInCacheDescribe(v *vm.VM) bool {
	if v == nil {
		return false
	}
	switch v.Status {
	case vm.StateTerminated:
		return false
	case vm.StateStopped:
		return v.OperatorStopped()
	default:
		return true
	}
}

// DescribeFromCache answers a DescribeInstances-shaped request straight from
// cache, sharing ParseInstanceListSelection/Reservations with the KV path so
// the two can never project a field or apply visibility differently.
//
// out == nil with a nil err is "no answer": the cache cannot support the
// claim a listing would be making, and the caller must not read that as an
// empty result. out != nil (even with zero Reservations) is a real, if
// possibly empty, answer. err != nil is a request the cache path itself could
// not parse (a malformed ID or an unknown filter) — a deterministic client
// error, independent of the cache's own state.
//
// An explicit InstanceIds request resolves each ID with Get, which answers
// definitively for one key whether or not the cache has completed its first
// whole-set sync — a request that names its instances is not held hostage by
// the readiness gate a completeness claim over the whole account needs. A
// filters-only request has no single key to resolve and falls back to List,
// gated on readiness because completeness is exactly what it would be
// claiming. degraded reports whether the cache was Degraded() at the moment
// of this read; the caller does not act on it yet.
func DescribeFromCache(ctx context.Context, cache CacheReader, input *ec2.DescribeInstancesInput, accountID, az string) (out *ec2.DescribeInstancesOutput, degraded bool, err error) {
	if cache == nil {
		return nil, false, nil
	}

	sel, err := handlers_ec2_instance.ParseInstanceListSelection(ctx, input, accountID, describeCacheOpName)
	if err != nil {
		return nil, false, err
	}

	degraded = cache.Degraded()

	var instances []*vm.VM
	if len(input.InstanceIds) > 0 {
		for _, id := range input.InstanceIds {
			if id == nil || *id == "" {
				continue
			}
			v, gerr := cache.Get(ctx, *id)
			if gerr != nil {
				return nil, false, gerr
			}
			if includeInCacheDescribe(v) {
				instances = append(instances, v)
			}
		}
	} else {
		list, ready := cache.List(ctx, accountID)
		if !ready {
			return nil, false, nil
		}
		for _, v := range list {
			if includeInCacheDescribe(v) {
				instances = append(instances, v)
			}
		}
	}

	result := sel.Reservations(ctx, instances, az, describeFallbackStateCode, describeFallbackStateName)
	return result, degraded, nil
}
