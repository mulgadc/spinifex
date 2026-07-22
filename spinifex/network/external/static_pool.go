package external

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const (
	// KVBucketStaticPool persists static-pool allocations. The bucket name
	// is unchanged from the pre-Q1 ExternalIPAM so existing cluster data
	// carries over with no manual migration.
	KVBucketStaticPool        = "spinifex-external-ipam"
	KVBucketStaticPoolVersion = 2

	staticPoolCASRetries = 5

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

// StaticPoolAllocator implements Allocator backed by a NATS JetStream KV
// bucket. Bucket schema and CAS semantics are unchanged from the pre-Q1
// ExternalIPAM.
type StaticPoolAllocator struct {
	kv    nats.KeyValue
	pools []ExternalPoolConfig
}

var _ Allocator = (*StaticPoolAllocator)(nil)

// NewStaticPoolAllocator creates the KV bucket (if missing), runs pending
// migrations, and seeds each pool's record.
func NewStaticPoolAllocator(js nats.JetStreamContext, pools []ExternalPoolConfig) (*StaticPoolAllocator, error) {
	kv, err := utils.GetOrCreateKVBucket(js, KVBucketStaticPool, 5)
	if err != nil {
		return nil, fmt.Errorf("create external IPAM KV bucket: %w", err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketStaticPool, kv, KVBucketStaticPoolVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketStaticPool, err)
	}
	a := &StaticPoolAllocator{kv: kv, pools: pools}
	if err := a.initPools(); err != nil {
		return nil, fmt.Errorf("init external IPAM pools: %w", err)
	}
	return a, nil
}

// NewStaticPoolAllocatorWithKV is the test constructor — skips bucket
// creation and migrations.
func NewStaticPoolAllocatorWithKV(kv nats.KeyValue, pools []ExternalPoolConfig) *StaticPoolAllocator {
	return &StaticPoolAllocator{kv: kv, pools: pools}
}

// KV exposes the underlying bucket so callers that share the bucket
// (notably ExternalIPAM facade tests) can construct sibling allocators.
func (a *StaticPoolAllocator) KV() nats.KeyValue { return a.kv }

// Pools returns the allocator's pool list. Used by the ExternalIPAM
// facade to satisfy pool-by-region lookups.
func (a *StaticPoolAllocator) Pools() []ExternalPoolConfig { return a.pools }

// Allocate implements Allocator.
func (a *StaticPoolAllocator) Allocate(_ context.Context, req AllocateRequest) (netip.Addr, error) {
	for attempt := range staticPoolCASRetries {
		record, revision, err := a.getRecord(req.PoolName)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("get external IPAM record: %w", err)
		}

		ip, err := nextAvailableIP(record)
		if err != nil {
			return netip.Addr{}, err
		}

		record.Allocated[ip] = ExternalIPAllocation{
			Purpose:      req.Purpose,
			AllocationID: req.AllocationID,
			ENIId:        req.ENIID,
			InstanceId:   req.InstanceID,
		}
		record.Cursor = ip

		data, err := json.Marshal(record)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("marshal external IPAM record: %w", err)
		}

		if _, err := a.kv.Update(req.PoolName, data, revision); err != nil {
			slog.Debug("external IPAM CAS conflict, retrying", "pool", req.PoolName, "attempt", attempt)
			continue
		}

		addr, ok := netip.AddrFromSlice(net.ParseIP(ip).To4())
		if !ok {
			return netip.Addr{}, fmt.Errorf("parse allocated ip %q", ip)
		}
		slog.Info("external IPAM allocated IP", "pool", req.PoolName, "ip", ip, "purpose", req.Purpose)
		return addr.Unmap(), nil
	}
	return netip.Addr{}, fmt.Errorf("external IPAM allocation failed after CAS retries for pool %s", req.PoolName)
}

// Release implements Allocator.
func (a *StaticPoolAllocator) Release(_ context.Context, poolName string, ip netip.Addr, ownerENIID string) error {
	target := ip.String()
	for attempt := range staticPoolCASRetries {
		record, revision, err := a.getRecord(poolName)
		if err != nil {
			return fmt.Errorf("get external IPAM record for release: %w", err)
		}

		alloc, ok := record.Allocated[target]
		if !ok {
			// An owner-scoped release of an already-freed IP is an idempotent
			// no-op (a duplicated or stale teardown), not an error.
			if ownerENIID != "" {
				return nil
			}
			return fmt.Errorf("IP %s not allocated in pool %s", target, poolName)
		}
		// Ownership guard: when a caller names an owner, only free the lease if
		// it still belongs to that ENI. External IPs are recycled and GC/teardown
		// sweeps re-emit releases, so a release for a prior owner must not free an
		// IP since reassigned to a live instance (that would double-allocate).
		if ownerENIID != "" && alloc.ENIId != "" && alloc.ENIId != ownerENIID {
			slog.Info("external IPAM release skip — IP reassigned to a different ENI (stale release)",
				"pool", poolName, "ip", target, "stale_owner_eni", ownerENIID, "current_owner_eni", alloc.ENIId)
			return nil
		}
		if alloc.Purpose == purposeIGWLRP {
			return fmt.Errorf("cannot release gateway IP %s in pool %s", target, poolName)
		}

		delete(record.Allocated, target)

		data, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("marshal external IPAM record: %w", err)
		}

		if _, err := a.kv.Update(poolName, data, revision); err != nil {
			slog.Debug("external IPAM release CAS conflict, retrying", "pool", poolName, "attempt", attempt)
			continue
		}

		slog.Info("external IPAM released IP", "pool", poolName, "ip", target)
		return nil
	}
	return fmt.Errorf("external IPAM release failed after CAS retries for pool %s", poolName)
}

// GetPoolRecord returns the current pool record.
func (a *StaticPoolAllocator) GetPoolRecord(poolName string) (*PoolRecord, error) {
	rec, _, err := a.getRecord(poolName)
	return rec, err
}

func (a *StaticPoolAllocator) initPools() error {
	for _, pool := range a.pools {
		if err := a.initPool(pool); err != nil {
			return fmt.Errorf("init pool %q: %w", pool.Name, err)
		}
	}
	return nil
}

func (a *StaticPoolAllocator) initPool(pool ExternalPoolConfig) error {
	chk, revision, err := a.getRecord(pool.Name)

	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
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

			data, err := json.Marshal(chk)
			if err != nil {
				return fmt.Errorf("marshal external IPAM record: %w", err)
			}

			if _, err := a.kv.Update(pool.Name, data, revision); err != nil {
				slog.Warn("external IPAM update failed", "pool", pool.Name, "err", err)
				return err
			}
			return nil
		}
		slog.Debug("external IPAM pool already initialized", "pool", pool.Name)
		return nil
	}

	slog.Info("external IPAM pool not found, creating", "pool", pool.Name)

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

	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal pool record: %w", err)
	}

	if _, err := a.kv.Create(pool.Name, data); err != nil {
		if errors.Is(err, nats.ErrKeyExists) {
			return nil
		}
		return fmt.Errorf("create pool KV entry: %w", err)
	}

	slog.Info("external IPAM pool initialized", "pool", pool.Name, "gateway_ip", gwIP)
	return nil
}

func (a *StaticPoolAllocator) getRecord(poolName string) (*PoolRecord, uint64, error) {
	entry, err := a.kv.Get(poolName)
	if err != nil {
		return nil, 0, err
	}

	var record PoolRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, 0, fmt.Errorf("unmarshal external IPAM record: %w", err)
	}
	return &record, entry.Revision(), nil
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
