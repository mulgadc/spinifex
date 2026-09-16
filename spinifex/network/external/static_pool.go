package external

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// KVBucketStaticPool persists static-pool allocations. The bucket name
	// is unchanged from the pre-Q1 ExternalIPAM so existing cluster data
	// carries over with no manual migration.
	KVBucketStaticPool        = "spinifex-external-ipam"
	KVBucketStaticPoolVersion = 2
	KVBucketStaticPoolHistory = 5

	// One pool record is one key, so every concurrent allocation in a pool
	// contends on it. A launch asking for sixteen public IPs is sixteen
	// writers, and each round has exactly one winner.
	staticPoolCASRetries = 25

	// purposeIGWLRP duplicates handlers/ec2/vpc.PurposeIGWLRP so the
	// allocator can reserve the gateway slot without importing handlers.
	// The string value is frozen by migration 002 — do not change it.
	purposeIGWLRP = "igw-lrp"
)

// ExternalIPAllocation describes how an external IP is being used.
type ExternalIPAllocation struct {
	Purpose      string `json:"purpose"`
	AllocationID string `json:"allocation_id,omitempty"`
	Association  string `json:"association,omitempty"`
	ENIId        string `json:"eni_id,omitempty"`
	InstanceId   string `json:"instance_id,omitempty"`
	Note         string `json:"note,omitempty"`
}

// PoolRecord tracks allocated external IPs for a single pool.
type PoolRecord struct {
	PoolName        string                          `json:"pool_name"`
	RangeStart      string                          `json:"range_start"`
	RangeEnd        string                          `json:"range_end"`
	Gateway         string                          `json:"gateway"`
	GatewayIP       string                          `json:"gateway_ip"`
	PrefixLen       int                             `json:"prefix_len"`
	Region          string                          `json:"region,omitempty"`
	AZ              string                          `json:"az,omitempty"`
	GwLrpRangeStart string                          `json:"gw_lrp_range_start,omitempty"`
	GwLrpRangeEnd   string                          `json:"gw_lrp_range_end,omitempty"`
	Allocated       map[string]ExternalIPAllocation `json:"allocated"`
	// Cursor is the last address handed out; allocation resumes just past it,
	// wrapping at RangeEnd so a released IP is not immediately reused.
	Cursor string `json:"cursor,omitempty"`
}

// staticPoolConfig describes the pool bucket. The name and history are
// unchanged from the pre-Q1 ExternalIPAM so existing cluster data carries over,
// and the migration runs on every open rather than only the first.
func staticPoolConfig() kvstore.Config {
	return kvstore.Config{
		Name:     KVBucketStaticPool,
		History:  KVBucketStaticPoolHistory,
		Attempts: staticPoolCASRetries,
		Missing:  "external IPAM: no JetStream client configured",
		OnOpen: func(ctx context.Context, kv jetstream.KeyValue) error {
			return migrate.DefaultRegistry.RunKV(ctx, KVBucketStaticPool, kv, KVBucketStaticPoolVersion)
		},
		Exhausted: func(key string, attempts int) error {
			return fmt.Errorf("external IPAM: pool %s contended after %d attempts", key, attempts)
		},
	}
}

// StaticPoolAllocator implements Allocator backed by a NATS JetStream KV
// bucket. Bucket schema and CAS semantics are unchanged from the pre-Q1
// ExternalIPAM.
type StaticPoolAllocator struct {
	store *kvstore.Store[PoolRecord]
	pools []ExternalPoolConfig
}

var _ Allocator = (*StaticPoolAllocator)(nil)

// NewStaticPoolAllocator creates the KV bucket (if missing), runs pending
// migrations, and seeds each pool's record.
func NewStaticPoolAllocator(ctx context.Context, js jetstream.JetStream, pools []ExternalPoolConfig) (*StaticPoolAllocator, error) {
	a := &StaticPoolAllocator{store: kvstore.New[PoolRecord](js, staticPoolConfig()), pools: pools}
	if err := a.initPools(ctx); err != nil {
		return nil, fmt.Errorf("init external IPAM pools: %w", err)
	}
	return a, nil
}

// NewStaticPoolAllocatorWithKV is the test constructor — skips bucket
// creation and migrations.
func NewStaticPoolAllocatorWithKV(kv jetstream.KeyValue, pools []ExternalPoolConfig) *StaticPoolAllocator {
	return &StaticPoolAllocator{store: kvstore.Over[PoolRecord](nil, kv, staticPoolConfig()), pools: pools}
}

// KV exposes the underlying bucket so callers that share it (the ExternalIPAM
// facade and its tests) can construct sibling allocators over the same handle.
func (a *StaticPoolAllocator) KV(ctx context.Context) (jetstream.KeyValue, error) {
	return a.store.KV(ctx)
}

// Pools returns the allocator's pool list. Used by the ExternalIPAM
// facade to satisfy pool-by-region lookups.
func (a *StaticPoolAllocator) Pools() []ExternalPoolConfig { return a.pools }

// Allocate implements Allocator.
func (a *StaticPoolAllocator) Allocate(ctx context.Context, req AllocateRequest) (netip.Addr, error) {
	var ip string
	err := a.store.Mutate(ctx, req.PoolName, func(record *PoolRecord) (bool, error) {
		// A record written before the field existed decodes to a nil map, which
		// the assignment below would panic on.
		if record.Allocated == nil {
			record.Allocated = map[string]ExternalIPAllocation{}
		}
		next, err := nextAvailableIP(record)
		if err != nil {
			return false, err
		}
		record.Allocated[next] = ExternalIPAllocation{
			Purpose:      req.Purpose,
			AllocationID: req.AllocationID,
			ENIId:        req.ENIID,
			InstanceId:   req.InstanceID,
		}
		record.Cursor = next
		ip = next
		return true, nil
	})
	if err != nil {
		return netip.Addr{}, err
	}

	addr, ok := netip.AddrFromSlice(net.ParseIP(ip).To4())
	if !ok {
		return netip.Addr{}, fmt.Errorf("parse allocated ip %q", ip)
	}
	slog.InfoContext(ctx, "external IPAM allocated IP", "pool", req.PoolName, "ip", ip, "purpose", req.Purpose)
	return addr.Unmap(), nil
}

// Release implements Allocator.
func (a *StaticPoolAllocator) Release(ctx context.Context, poolName string, ip netip.Addr, ownerENIID string) error {
	target := ip.String()
	var released bool
	err := a.store.Mutate(ctx, poolName, func(record *PoolRecord) (bool, error) {
		// Reset per attempt: a retry that finds the IP already gone must not
		// inherit the previous attempt's verdict.
		released = false

		alloc, ok := record.Allocated[target]
		if !ok {
			// An owner-scoped release of an already-freed IP is an idempotent
			// no-op (a duplicated or stale teardown), not an error.
			if ownerENIID != "" {
				return false, nil
			}
			return false, fmt.Errorf("IP %s not allocated in pool %s", target, poolName)
		}
		// Ownership guard: when a caller names an owner, only free the lease if
		// it still belongs to that ENI. External IPs are recycled and GC/teardown
		// sweeps re-emit releases, so a release for a prior owner must not free an
		// IP since reassigned to a live instance (that would double-allocate).
		if ownerENIID != "" && alloc.ENIId != "" && alloc.ENIId != ownerENIID {
			slog.InfoContext(ctx, "external IPAM release skip — IP reassigned to a different ENI (stale release)",
				"pool", poolName, "ip", target, "stale_owner_eni", ownerENIID, "current_owner_eni", alloc.ENIId)
			return false, nil
		}
		if alloc.Purpose == purposeIGWLRP {
			return false, fmt.Errorf("cannot release gateway IP %s in pool %s", target, poolName)
		}

		delete(record.Allocated, target)
		released = true
		return true, nil
	})
	if err != nil {
		return err
	}
	if released {
		slog.InfoContext(ctx, "external IPAM released IP", "pool", poolName, "ip", target)
	}
	return nil
}

// GetPoolRecord returns the current pool record.
func (a *StaticPoolAllocator) GetPoolRecord(ctx context.Context, poolName string) (*PoolRecord, error) {
	rec, _, err := a.store.Get(ctx, poolName)
	return rec, err
}

func (a *StaticPoolAllocator) initPools(ctx context.Context) error {
	for _, pool := range a.pools {
		if err := a.initPool(ctx, pool); err != nil {
			return fmt.Errorf("init pool %q: %w", pool.Name, err)
		}
	}
	return nil
}

func (a *StaticPoolAllocator) initPool(ctx context.Context, pool ExternalPoolConfig) error {
	chk, revision, err := a.store.Get(ctx, pool.Name)

	if err != nil && !errors.Is(err, kvstore.ErrNotFound) {
		return err
	}

	if err == nil {
		if chk.RangeStart != pool.RangeStart || chk.RangeEnd != pool.RangeEnd ||
			chk.GwLrpRangeStart != pool.GwLrpRangeStart || chk.GwLrpRangeEnd != pool.GwLrpRangeEnd {
			slog.Info("external IPAM pool config drift, reconciling KV",
				"pool", pool.Name,
				"old_range", chk.RangeStart+"-"+chk.RangeEnd, "new_range", pool.RangeStart+"-"+pool.RangeEnd,
				"old_gw_lrp_range", chk.GwLrpRangeStart+"-"+chk.GwLrpRangeEnd,
				"new_gw_lrp_range", pool.GwLrpRangeStart+"-"+pool.GwLrpRangeEnd)

			chk.RangeStart = pool.RangeStart
			chk.RangeEnd = pool.RangeEnd
			chk.GwLrpRangeStart = pool.GwLrpRangeStart
			chk.GwLrpRangeEnd = pool.GwLrpRangeEnd

			// Losing the CAS is another node reconciling the same drift, which
			// the Create branch below already treats as success for the same
			// reason. Failing the boot over it would take this node down.
			if _, err := a.store.CompareAndSet(ctx, pool.Name, chk, revision); err != nil &&
				!errors.Is(err, kvstore.ErrConflict) {
				slog.WarnContext(ctx, "external IPAM update failed", "pool", pool.Name, "err", err)
				return err
			}
			return nil
		}
		slog.DebugContext(ctx, "external IPAM pool already initialized", "pool", pool.Name)
		return nil
	}

	slog.InfoContext(ctx, "external IPAM pool not found, creating", "pool", pool.Name)

	gwIP := pool.GatewayIP
	if gwIP == "" {
		gwIP = pool.RangeStart
	}

	record := &PoolRecord{
		PoolName:        pool.Name,
		RangeStart:      pool.RangeStart,
		RangeEnd:        pool.RangeEnd,
		Gateway:         pool.Gateway,
		GatewayIP:       gwIP,
		PrefixLen:       pool.PrefixLen,
		Region:          pool.Region,
		AZ:              pool.AZ,
		GwLrpRangeStart: pool.GwLrpRangeStart,
		GwLrpRangeEnd:   pool.GwLrpRangeEnd,
		Allocated: map[string]ExternalIPAllocation{
			gwIP: {Purpose: purposeIGWLRP, Note: "OVN router SNAT address"},
		},
	}

	if _, err := a.store.Create(ctx, pool.Name, record); err != nil {
		if errors.Is(err, kvstore.ErrExists) {
			return nil
		}
		return fmt.Errorf("create pool KV entry: %w", err)
	}

	slog.InfoContext(ctx, "external IPAM pool initialized", "pool", pool.Name, "gateway_ip", gwIP)
	return nil
}

// nextAvailableIP picks the next unallocated address, skipping [GwLrpRangeStart,
// GwLrpRangeEnd]. Resumes one past Cursor, wrapping at RangeEnd.
func nextAvailableIP(record *PoolRecord) (string, error) {
	startIP := net.ParseIP(record.RangeStart).To4()
	endIP := net.ParseIP(record.RangeEnd).To4()
	if startIP == nil || endIP == nil {
		return "", fmt.Errorf("invalid IP range: %s - %s", record.RangeStart, record.RangeEnd)
	}

	var gwLrpStart, gwLrpEnd uint32
	hasGwLrp := false
	if record.GwLrpRangeStart != "" && record.GwLrpRangeEnd != "" {
		s := net.ParseIP(record.GwLrpRangeStart).To4()
		e := net.ParseIP(record.GwLrpRangeEnd).To4()
		if s != nil && e != nil {
			gwLrpStart = ipv4ToUint32(s)
			gwLrpEnd = ipv4ToUint32(e)
			hasGwLrp = true
		}
	}

	startInt := ipv4ToUint32(startIP)
	endInt := ipv4ToUint32(endIP)
	if endInt < startInt {
		return "", fmt.Errorf("invalid IP range: %s - %s", record.RangeStart, record.RangeEnd)
	}
	total := endInt - startInt + 1

	// Empty or out-of-range cursor starts at RangeStart.
	offset := uint32(0)
	if c := net.ParseIP(record.Cursor).To4(); c != nil {
		ci := ipv4ToUint32(c)
		if ci >= startInt && ci <= endInt {
			offset = ((ci - startInt) + 1) % total
		}
	}

	for k := range total {
		i := startInt + (offset+k)%total
		if hasGwLrp && i >= gwLrpStart && i <= gwLrpEnd {
			continue
		}
		candidate := uint32ToIPv4(i).String()
		if _, taken := record.Allocated[candidate]; !taken {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("pool %s exhausted: %w", record.PoolName, errors.New(awserrors.ErrorInsufficientAddressCapacity))
}
