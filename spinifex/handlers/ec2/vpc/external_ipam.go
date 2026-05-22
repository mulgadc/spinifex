package handlers_ec2_vpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"

	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const (
	KVBucketExternalIPAM        = "spinifex-external-ipam"
	KVBucketExternalIPAMVersion = 1
)

// ExternalIPAllocation describes how an external IP is being used.
type ExternalIPAllocation struct {
	Type         string `json:"type"`                    // "gateway", "auto_assign", "elastic_ip"
	AllocationID string `json:"allocation_id,omitempty"` // For elastic IPs
	Association  string `json:"association,omitempty"`   // ENI ID for elastic IPs
	ENIId        string `json:"eni_id,omitempty"`        // ENI owning this IP
	InstanceId   string `json:"instance_id,omitempty"`   // Instance owning this IP
	Note         string `json:"note,omitempty"`          // Human-readable note
}

// ExternalIPAMRecord tracks allocated external IPs for a single pool.
type ExternalIPAMRecord struct {
	PoolName   string `json:"pool_name"`
	RangeStart string `json:"range_start"`
	RangeEnd   string `json:"range_end"`
	Gateway    string `json:"gateway"`
	GatewayIP  string `json:"gateway_ip"`
	PrefixLen  int    `json:"prefix_len"`
	Region     string `json:"region,omitempty"`
	AZ         string `json:"az,omitempty"`
	// GwLrpRangeStart/End mirrors ExternalPoolConfig — IPAM skips this
	// sub-range so it doesn't collide with vpcd's gateway LRP IPs in
	// centralized NAT (mulga-siv-36).
	GwLrpRangeStart string                          `json:"gw_lrp_range_start,omitempty"`
	GwLrpRangeEnd   string                          `json:"gw_lrp_range_end,omitempty"`
	Allocated       map[string]ExternalIPAllocation `json:"allocated"`
}

// ExternalPoolConfig is the admin-defined pool from spinifex.toml.
type ExternalPoolConfig struct {
	Name       string
	RangeStart string
	RangeEnd   string
	Gateway    string
	GatewayIP  string
	PrefixLen  int
	Region     string
	AZ         string
	// GwLrpRangeStart/End reserves a sub-range of the LAN for OVN gateway
	// LRP IPs in centralized NAT mode (mulga-siv-36). IPAM must skip these
	// addresses or the per-VM EIP allocator and vpcd will fight over them.
	GwLrpRangeStart string
	GwLrpRangeEnd   string
}

// ExternalIPAM manages external IP allocation from admin-defined pools using NATS KV with CAS.
type ExternalIPAM struct {
	kv    nats.KeyValue
	pools []ExternalPoolConfig
}

// NewExternalIPAM creates a new ExternalIPAM backed by NATS JetStream KV.
func NewExternalIPAM(js nats.JetStreamContext, pools []ExternalPoolConfig) (*ExternalIPAM, error) {
	kv, err := utils.GetOrCreateKVBucket(js, KVBucketExternalIPAM, 5)
	if err != nil {
		return nil, fmt.Errorf("create external IPAM KV bucket: %w", err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketExternalIPAM, kv, KVBucketExternalIPAMVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketExternalIPAM, err)
	}
	ipam := &ExternalIPAM{kv: kv, pools: pools}
	if err := ipam.initPools(); err != nil {
		return nil, fmt.Errorf("init external IPAM pools: %w", err)
	}
	return ipam, nil
}

// NewExternalIPAMWithKV creates an ExternalIPAM with an existing KV bucket (for testing).
func NewExternalIPAMWithKV(kv nats.KeyValue, pools []ExternalPoolConfig) *ExternalIPAM {
	return &ExternalIPAM{kv: kv, pools: pools}
}

// initPools ensures each configured pool has a KV record. Idempotent — safe to call
// on every vpcd startup. Reserves the gateway IP in each pool.
func (m *ExternalIPAM) initPools() error {
	for _, pool := range m.pools {
		if err := m.initPool(pool); err != nil {
			return fmt.Errorf("init pool %q: %w", pool.Name, err)
		}
	}
	return nil
}

func (m *ExternalIPAM) initPool(pool ExternalPoolConfig) error {
	chk, revision, err := m.getRecord(pool.Name)

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

			if _, err := m.kv.Update(pool.Name, data, revision); err != nil {
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

	record := &ExternalIPAMRecord{
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
			gwIP: {Type: "gateway", Note: "OVN router SNAT address"},
		},
	}

	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal pool record: %w", err)
	}

	if _, err := m.kv.Create(pool.Name, data); err != nil {
		// Another instance may have initialized concurrently — that's fine.
		if errors.Is(err, nats.ErrKeyExists) {
			return nil
		}
		return fmt.Errorf("create pool KV entry: %w", err)
	}

	slog.Info("external IPAM pool initialized", "pool", pool.Name, "gateway_ip", gwIP)
	return nil
}

// AllocateIP allocates the next available external IP from the best pool
// matching the given region/AZ. Returns the allocated IP and pool name.
// allocID is the EIP allocation ID (for elastic IPs); pass "" for other types.
func (m *ExternalIPAM) AllocateIP(region, az, allocType, allocID, eniID, instanceID string) (string, string, error) {
	pool := m.findPool(region, az)
	if pool == nil {
		return "", "", fmt.Errorf("InsufficientAddressCapacity: no external pool available for region=%q az=%q", region, az)
	}
	ip, err := m.allocateFromPool(pool.Name, allocType, allocID, eniID, instanceID)
	if err != nil {
		return "", "", err
	}
	return ip, pool.Name, nil
}

// AllocateFromPool allocates an IP from a specific named pool.
func (m *ExternalIPAM) AllocateFromPool(poolName, allocType, allocID, eniID, instanceID string) (string, error) {
	return m.allocateFromPool(poolName, allocType, allocID, eniID, instanceID)
}

func (m *ExternalIPAM) allocateFromPool(poolName, allocType, allocID, eniID, instanceID string) (string, error) {
	for attempt := range 5 {
		record, revision, err := m.getRecord(poolName)
		if err != nil {
			return "", fmt.Errorf("get external IPAM record: %w", err)
		}

		ip, err := nextAvailableExternalIP(record)
		if err != nil {
			return "", err
		}

		record.Allocated[ip] = ExternalIPAllocation{
			Type:         allocType,
			AllocationID: allocID,
			ENIId:        eniID,
			InstanceId:   instanceID,
		}

		data, err := json.Marshal(record)
		if err != nil {
			return "", fmt.Errorf("marshal external IPAM record: %w", err)
		}

		if _, err := m.kv.Update(poolName, data, revision); err != nil {
			slog.Debug("external IPAM CAS conflict, retrying", "pool", poolName, "attempt", attempt)
			continue
		}

		slog.Info("external IPAM allocated IP", "pool", poolName, "ip", ip, "type", allocType)
		return ip, nil
	}

	return "", fmt.Errorf("external IPAM allocation failed after CAS retries for pool %s", poolName)
}

// ReleaseIP releases a previously allocated external IP back to its pool.
func (m *ExternalIPAM) ReleaseIP(poolName, ip string) error {
	for attempt := range 5 {
		record, revision, err := m.getRecord(poolName)
		if err != nil {
			return fmt.Errorf("get external IPAM record for release: %w", err)
		}

		alloc, ok := record.Allocated[ip]
		if !ok {
			return fmt.Errorf("IP %s not allocated in pool %s", ip, poolName)
		}
		if alloc.Type == "gateway" {
			return fmt.Errorf("cannot release gateway IP %s in pool %s", ip, poolName)
		}

		delete(record.Allocated, ip)

		data, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("marshal external IPAM record: %w", err)
		}

		if _, err := m.kv.Update(poolName, data, revision); err != nil {
			slog.Debug("external IPAM release CAS conflict, retrying", "pool", poolName, "attempt", attempt)
			continue
		}

		slog.Info("external IPAM released IP", "pool", poolName, "ip", ip)
		return nil
	}

	return fmt.Errorf("external IPAM release failed after CAS retries for pool %s", poolName)
}

// GetPoolRecord returns the current IPAM record for a pool.
func (m *ExternalIPAM) GetPoolRecord(poolName string) (*ExternalIPAMRecord, error) {
	record, _, err := m.getRecord(poolName)
	return record, err
}

// findPool returns the best pool for the given region/AZ using the same
// fallback order as topology.go: AZ-scoped → region-scoped → unscoped.
func (m *ExternalIPAM) findPool(region, az string) *ExternalPoolConfig {
	// 1. AZ-scoped match
	for i := range m.pools {
		p := &m.pools[i]
		if p.AZ != "" && p.AZ == az && p.Region == region {
			return p
		}
	}
	// 2. Region-scoped (no AZ)
	for i := range m.pools {
		p := &m.pools[i]
		if p.AZ == "" && p.Region != "" && p.Region == region {
			return p
		}
	}
	// 3. Unscoped (global pool)
	for i := range m.pools {
		p := &m.pools[i]
		if p.Region == "" && p.AZ == "" {
			return p
		}
	}
	return nil
}

func (m *ExternalIPAM) getRecord(poolName string) (*ExternalIPAMRecord, uint64, error) {
	entry, err := m.kv.Get(poolName)
	if err != nil {
		return nil, 0, err
	}

	var record ExternalIPAMRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, 0, fmt.Errorf("unmarshal external IPAM record: %w", err)
	}

	return &record, entry.Revision(), nil
}

// nextAvailableExternalIP finds the next unallocated IP in the pool's
// range. Addresses inside [GwLrpRangeStart, GwLrpRangeEnd] are skipped —
// vpcd reserves them for OVN gateway LRPs (mulga-siv-36).
func nextAvailableExternalIP(record *ExternalIPAMRecord) (string, error) {
	startIP := net.ParseIP(record.RangeStart).To4()
	endIP := net.ParseIP(record.RangeEnd).To4()
	if startIP == nil || endIP == nil {
		return "", fmt.Errorf("invalid IP range: %s - %s", record.RangeStart, record.RangeEnd)
	}

	var gwLrpStart, gwLrpEnd int64 = -1, -1
	if record.GwLrpRangeStart != "" && record.GwLrpRangeEnd != "" {
		s := net.ParseIP(record.GwLrpRangeStart).To4()
		e := net.ParseIP(record.GwLrpRangeEnd).To4()
		if s != nil && e != nil {
			gwLrpStart = ipToInt(s).Int64()
			gwLrpEnd = ipToInt(e).Int64()
		}
	}

	startInt := ipToInt(startIP)
	endInt := ipToInt(endIP)

	for i := startInt.Int64(); i <= endInt.Int64(); i++ {
		if gwLrpStart >= 0 && i >= gwLrpStart && i <= gwLrpEnd {
			continue
		}
		candidate := intToIP(intFromInt64(i)).String()
		if _, taken := record.Allocated[candidate]; !taken {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("InsufficientAddressCapacity: pool %s exhausted", record.PoolName)
}

// ValidatePoolConfig checks that a pool config is valid.
func ValidatePoolConfig(pool ExternalPoolConfig) error {
	if pool.Name == "" {
		return fmt.Errorf("pool name is required")
	}
	if pool.Gateway == "" {
		return fmt.Errorf("gateway is required for pool %q", pool.Name)
	}
	if net.ParseIP(pool.Gateway) == nil {
		return fmt.Errorf("invalid gateway IP: %q", pool.Gateway)
	}
	if pool.GatewayIP != "" && net.ParseIP(pool.GatewayIP) == nil {
		return fmt.Errorf("invalid gateway_ip: %q", pool.GatewayIP)
	}
	startIP := net.ParseIP(pool.RangeStart)
	if startIP == nil {
		return fmt.Errorf("invalid range_start: %q", pool.RangeStart)
	}
	endIP := net.ParseIP(pool.RangeEnd)
	if endIP == nil {
		return fmt.Errorf("invalid range_end: %q", pool.RangeEnd)
	}
	if ipToInt(startIP.To4()).Cmp(ipToInt(endIP.To4())) > 0 {
		return fmt.Errorf("range_start %s is greater than range_end %s", pool.RangeStart, pool.RangeEnd)
	}
	// gw_lrp_range must be valid IPs and must NOT overlap range_start/end
	// — otherwise vpcd's gateway LRP allocator and per-VM EIP allocator
	// would fight over the same address (mulga-siv-36).
	if pool.GwLrpRangeStart != "" || pool.GwLrpRangeEnd != "" {
		gwS := net.ParseIP(pool.GwLrpRangeStart)
		gwE := net.ParseIP(pool.GwLrpRangeEnd)
		if gwS == nil {
			return fmt.Errorf("invalid gw_lrp_range_start: %q", pool.GwLrpRangeStart)
		}
		if gwE == nil {
			return fmt.Errorf("invalid gw_lrp_range_end: %q", pool.GwLrpRangeEnd)
		}
		gwSi := ipToInt(gwS.To4())
		gwEi := ipToInt(gwE.To4())
		if gwSi.Cmp(gwEi) > 0 {
			return fmt.Errorf("gw_lrp_range_start %s is greater than gw_lrp_range_end %s",
				pool.GwLrpRangeStart, pool.GwLrpRangeEnd)
		}
		rangeSi := ipToInt(startIP.To4())
		rangeEi := ipToInt(endIP.To4())
		// Overlap test: !(gwE < rangeS || gwS > rangeE)
		if gwEi.Cmp(rangeSi) >= 0 && gwSi.Cmp(rangeEi) <= 0 {
			return fmt.Errorf("gw_lrp_range %s-%s overlaps range %s-%s",
				pool.GwLrpRangeStart, pool.GwLrpRangeEnd, pool.RangeStart, pool.RangeEnd)
		}
	}
	return nil
}

// intFromInt64 wraps a plain int64 into a *big.Int-compatible IP conversion.
func intFromInt64(v int64) *big.Int {
	return big.NewInt(v)
}
