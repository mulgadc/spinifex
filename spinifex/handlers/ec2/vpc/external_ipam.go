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

	// DHCP-sourced metadata — set only for allocations whose pool has
	// source="dhcp" (see ObtainDHCPLease). These fields let describe
	// surfaces and operators trace a given IP back to the upstream lease
	// without re-querying vpcd.
	LeaseExpiresUnix int64  `json:"lease_expires_unix,omitempty"` // Absolute expiry from the server's ACK
	DHCPServerID     string `json:"dhcp_server_id,omitempty"`     // Option 54 of the ACK
	HWAddr           string `json:"hw_addr,omitempty"`            // chaddr placed in the DHCP payload
}

// ExternalIPAMRecord tracks allocated external IPs for a single pool.
type ExternalIPAMRecord struct {
	PoolName   string                          `json:"pool_name"`
	RangeStart string                          `json:"range_start"`
	RangeEnd   string                          `json:"range_end"`
	Gateway    string                          `json:"gateway"`
	GatewayIP  string                          `json:"gateway_ip"`
	PrefixLen  int                             `json:"prefix_len"`
	Region     string                          `json:"region,omitempty"`
	AZ         string                          `json:"az,omitempty"`
	Allocated  map[string]ExternalIPAllocation `json:"allocated"`
}

// ExternalPoolConfig is the admin-defined pool from spinifex.toml.
type ExternalPoolConfig struct {
	Name           string
	Source         string // "static" (default) or "dhcp"
	RangeStart     string
	RangeEnd       string
	Gateway        string
	GatewayIP      string
	PrefixLen      int
	Region         string
	AZ             string
	DhcpBindBridge string // Bridge where the DHCP AF_PACKET socket binds (e.g. "br-wan"). Linux bridge in veth mode; OVS bridge in direct mode. Never "br-ext".
}

// IsDHCP returns true if this pool obtains IPs from router DHCP.
func (p *ExternalPoolConfig) IsDHCP() bool {
	return p.Source == "dhcp"
}

// ExternalIPAM manages external IP allocation from admin-defined pools using NATS KV with CAS.
type ExternalIPAM struct {
	kv    nats.KeyValue
	pools []ExternalPoolConfig
	// nc is used to talk to spinifex-vpcd for DHCP-sourced pools via
	// vpc.dhcp.acquire / vpc.dhcp.release. May be nil when no pool has
	// Source="dhcp" (callers should still pass it if available).
	nc *nats.Conn
}

// NewExternalIPAM creates a new ExternalIPAM backed by NATS JetStream KV.
// nc is required for DHCP-sourced pools and may be nil for static-only
// deployments.
func NewExternalIPAM(nc *nats.Conn, js nats.JetStreamContext, pools []ExternalPoolConfig) (*ExternalIPAM, error) {
	kv, err := utils.GetOrCreateKVBucket(js, KVBucketExternalIPAM, 5)
	if err != nil {
		return nil, fmt.Errorf("create external IPAM KV bucket: %w", err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketExternalIPAM, kv, KVBucketExternalIPAMVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketExternalIPAM, err)
	}
	ipam := &ExternalIPAM{kv: kv, pools: pools, nc: nc}
	if err := ipam.initPools(); err != nil {
		return nil, fmt.Errorf("init external IPAM pools: %w", err)
	}
	return ipam, nil
}

// NewExternalIPAMWithKV creates an ExternalIPAM with an existing KV bucket
// (for testing). nc may be nil when the test exercises only static pools.
func NewExternalIPAMWithKV(nc *nats.Conn, kv nats.KeyValue, pools []ExternalPoolConfig) *ExternalIPAM {
	return &ExternalIPAM{kv: kv, pools: pools, nc: nc}
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
	_, _, err := m.getRecord(pool.Name)
	if err == nil {
		slog.Debug("external IPAM pool already initialized", "pool", pool.Name)
		return nil // Already exists
	}
	if !errors.Is(err, nats.ErrKeyNotFound) {
		return err
	}

	gwIP := pool.GatewayIP
	if gwIP == "" && !pool.IsDHCP() {
		gwIP = pool.RangeStart
	}

	// For DHCP pools, obtain the gateway IP from router DHCP if not set.
	// The gateway lease is long-lived: vpcd's renewal goroutine keeps it
	// refreshed until the pool is torn down.
	var gatewayLease *dhcpLeaseResult
	if gwIP == "" && pool.IsDHCP() {
		lease, dhcpErr := ObtainDHCPLease(m.nc, pool.DhcpBindBridge,
			"gateway-"+pool.Name,
			"gateway-"+pool.Name,
			"mulga-spinifex-gw",
			pool.Name,
		)
		if dhcpErr != nil {
			return fmt.Errorf("DHCP gateway lease for pool %q: %w", pool.Name, dhcpErr)
		}
		gatewayLease = &lease
		gwIP = lease.IP
		slog.Info("external IPAM obtained gateway IP via DHCP", "pool", pool.Name, "gateway_ip", gwIP)
	}

	gatewayAlloc := ExternalIPAllocation{Type: "gateway", Note: "OVN router SNAT address"}
	if gatewayLease != nil {
		gatewayAlloc.LeaseExpiresUnix = gatewayLease.ExpiresUnix
		gatewayAlloc.DHCPServerID = gatewayLease.ServerID
		gatewayAlloc.HWAddr = gatewayLease.HWAddr
	}

	record := &ExternalIPAMRecord{
		PoolName:   pool.Name,
		RangeStart: pool.RangeStart,
		RangeEnd:   pool.RangeEnd,
		Gateway:    pool.Gateway,
		GatewayIP:  gwIP,
		PrefixLen:  pool.PrefixLen,
		Region:     pool.Region,
		AZ:         pool.AZ,
		Allocated: map[string]ExternalIPAllocation{
			gwIP: gatewayAlloc,
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

	slog.Info("external IPAM pool initialized", "pool", pool.Name, "gateway_ip", gwIP, "source", pool.Source)
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
	pool := m.findPoolByName(poolName)

	// DHCP client-id: prefer allocID (EIPs), then eniID (ENIs), then instanceID.
	// At least one must be non-empty for DHCP pools — the upstream DHCP server
	// uses this to keep per-client leases distinct.
	clientID := allocID
	if clientID == "" {
		clientID = eniID
	}
	if clientID == "" {
		clientID = instanceID
	}

	// For DHCP-sourced pools: acquire the lease once, up front. vpcd's
	// Manager is idempotent on ClientID, so CAS retries below will not
	// trigger a second DORA — we only release at the end if no CAS
	// attempt succeeded.
	var dhcpLease *dhcpLeaseResult
	if pool != nil && pool.IsDHCP() {
		hostname, vendorClass := dhcpIdentityOptions(eniID, instanceID, poolName)
		lease, err := ObtainDHCPLease(m.nc, pool.DhcpBindBridge, clientID, hostname, vendorClass, poolName)
		if err != nil {
			return "", fmt.Errorf("DHCP lease for %s: %w", clientID, err)
		}
		dhcpLease = &lease
	}

	for attempt := range 5 {
		record, revision, err := m.getRecord(poolName)
		if err != nil {
			if dhcpLease != nil {
				_ = ReleaseDHCPLease(m.nc, clientID)
			}
			return "", fmt.Errorf("get external IPAM record: %w", err)
		}

		var ip string
		if dhcpLease != nil {
			ip = dhcpLease.IP
		} else {
			// Static source: pick next IP from range
			ip, err = nextAvailableExternalIP(record)
			if err != nil {
				return "", err
			}
		}

		alloc := ExternalIPAllocation{
			Type:         allocType,
			AllocationID: allocID,
			ENIId:        eniID,
			InstanceId:   instanceID,
		}
		if dhcpLease != nil {
			alloc.LeaseExpiresUnix = dhcpLease.ExpiresUnix
			alloc.DHCPServerID = dhcpLease.ServerID
			alloc.HWAddr = dhcpLease.HWAddr
		}
		record.Allocated[ip] = alloc

		data, err := json.Marshal(record)
		if err != nil {
			if dhcpLease != nil {
				_ = ReleaseDHCPLease(m.nc, clientID)
			}
			return "", fmt.Errorf("marshal external IPAM record: %w", err)
		}

		if _, err := m.kv.Update(poolName, data, revision); err != nil {
			slog.Debug("external IPAM CAS conflict, retrying", "pool", poolName, "attempt", attempt)
			continue
		}

		source := "static"
		if pool != nil {
			source = pool.Source
		}
		slog.Info("external IPAM allocated IP", "pool", poolName, "ip", ip, "type", allocType, "source", source)
		return ip, nil
	}

	// All CAS attempts exhausted — release any DHCP lease we acquired so
	// vpcd doesn't hold a binding for an IP we won't use.
	if dhcpLease != nil {
		_ = ReleaseDHCPLease(m.nc, clientID)
	}
	return "", fmt.Errorf("external IPAM allocation failed after CAS retries for pool %s", poolName)
}

// dhcpIdentityOptions composes the option 12 (hostname) and option 60
// (vendor class) values for a per-allocation lease. Empty eniID /
// instanceID fall through to identifiers that still group usefully on
// the upstream server's lease table. Instance and ENI identifiers are
// used as-is (they already carry their "i-" / "eni-" prefixes).
func dhcpIdentityOptions(eniID, instanceID, poolName string) (hostname, vendorClass string) {
	hostname = eniID
	if hostname == "" {
		if instanceID != "" {
			hostname = "spinifex-" + instanceID
		} else {
			hostname = "spinifex-" + poolName
		}
	}
	vendorClass = "mulga-spinifex"
	if instanceID != "" {
		vendorClass = instanceID
	}
	return hostname, vendorClass
}

// ReleaseIP releases a previously allocated external IP back to its pool.
func (m *ExternalIPAM) ReleaseIP(poolName, ip string) error {
	pool := m.findPoolByName(poolName)

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

		// Release DHCP lease if this is a DHCP-sourced pool. clientID
		// selection mirrors allocateFromPool so vpcd's Manager can find the
		// lease it originally acquired.
		if pool != nil && pool.IsDHCP() {
			clientID := alloc.AllocationID
			if clientID == "" {
				clientID = alloc.ENIId
			}
			if clientID == "" {
				clientID = alloc.InstanceId
			}
			if releaseErr := ReleaseDHCPLease(m.nc, clientID); releaseErr != nil {
				slog.Warn("Failed to release DHCP lease", "pool", poolName, "ip", ip, "err", releaseErr)
			}
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

// findPoolByName returns the pool config by name.
func (m *ExternalIPAM) findPoolByName(name string) *ExternalPoolConfig {
	for i := range m.pools {
		if m.pools[i].Name == name {
			return &m.pools[i]
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

// nextAvailableExternalIP finds the next unallocated IP in the pool's range.
func nextAvailableExternalIP(record *ExternalIPAMRecord) (string, error) {
	startIP := net.ParseIP(record.RangeStart).To4()
	endIP := net.ParseIP(record.RangeEnd).To4()
	if startIP == nil || endIP == nil {
		return "", fmt.Errorf("invalid IP range: %s - %s", record.RangeStart, record.RangeEnd)
	}

	startInt := ipToInt(startIP)
	endInt := ipToInt(endIP)

	for i := startInt.Int64(); i <= endInt.Int64(); i++ {
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
	// DHCP pools don't need range_start/range_end
	if !pool.IsDHCP() {
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
	}
	return nil
}

// intFromInt64 wraps a plain int64 into a *big.Int-compatible IP conversion.
func intFromInt64(v int64) *big.Int {
	return big.NewInt(v)
}
