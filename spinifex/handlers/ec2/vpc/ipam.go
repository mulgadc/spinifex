package handlers_ec2_vpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	KVBucketIPAM        = "spinifex-vpc-ipam"
	KVBucketIPAMVersion = 2
	KVBucketIPAMHistory = 5
)

// ipamCASAttempts bounds one allocation's retry on a revision conflict. Every
// address in a subnet shares one record, so a bulk RunInstances contends with
// itself once per instance; the default five is below a single realistic launch
// burst and returns exhaustion where the caller expects an address.
const ipamCASAttempts = 50

// ErrIPOutOfRange means the requested address is not a usable host address in
// the subnet: outside the CIDR, in the reserved head, or the broadcast address.
var ErrIPOutOfRange = errors.New("ip address not usable in subnet")

// ErrIPInUse means the requested address is already allocated in the subnet.
var ErrIPInUse = errors.New("ip address already allocated")

// IPEntry tags one IP allocation with its Purpose + owner (eni-, eipalloc-,
// etc) so multi-VPC clusters can reclaim/audit by (owner, purpose).
type IPEntry struct {
	IP      string `json:"ip"`
	Purpose string `json:"purpose"`            // one of the Purpose* constants
	OwnerID string `json:"owner_id,omitempty"` // ENI / EIP / NATGW / IGW resource ID
}

// IPAMRecord tracks allocated IPs for a subnet.
type IPAMRecord struct {
	SubnetId  string    `json:"subnet_id"`
	CidrBlock string    `json:"cidr_block"`
	Allocated []IPEntry `json:"allocated"`
}

// IPAM manages IP address allocation for VPC subnets using NATS KV with CAS.
type IPAM struct {
	store *kvstore.Store[IPAMRecord]
}

// ipamConfig describes the IPAM bucket. js is captured for the migration, which
// resolves the stream itself and so needs the client rather than the handle; a
// nil js is a caller supplying its own already-migrated handle.
func ipamConfig(js jetstream.JetStream) kvstore.Config {
	cfg := kvstore.Config{
		Name:     KVBucketIPAM,
		History:  KVBucketIPAMHistory,
		Missing:  "IPAM KV bucket not initialized",
		Attempts: ipamCASAttempts,
		Exhausted: func(key string, attempts int) error {
			return fmt.Errorf("IPAM CAS exhausted for subnet %s after %d attempts", key, attempts)
		},
	}
	if js != nil {
		cfg.OnOpen = func(ctx context.Context, kv jetstream.KeyValue) error {
			return migrate.DefaultRegistry.RunKVWithJetStream(ctx, KVBucketIPAM, kv, js, KVBucketIPAMVersion)
		}
	}
	return cfg
}

// NewIPAM creates a new IPAM instance backed by NATS JetStream KV.
func NewIPAM(ctx context.Context, js jetstream.JetStream) (*IPAM, error) {
	store := kvstore.New[IPAMRecord](js, ipamConfig(js))
	// Opened eagerly: a bucket that cannot be created must fail construction
	// rather than the first allocation that needs it.
	if _, err := store.KV(ctx); err != nil {
		return nil, fmt.Errorf("failed to create IPAM KV bucket: %w", err)
	}
	return &IPAM{store: store}, nil
}

// NewIPAMWithKV creates an IPAM with an existing KV bucket (for testing).
func NewIPAMWithKV(kv jetstream.KeyValue) *IPAM {
	return &IPAM{store: kvstore.Over[IPAMRecord](nil, kv, ipamConfig(nil))}
}

// AllocateIP allocates an IP from the subnet, reserving the first 4 and last
// addresses per AWS convention. Uses CAS for conflict-free multi-node allocation.
func (m *IPAM) AllocateIP(ctx context.Context, subnetId, cidrBlock, purpose, ownerID string) (string, error) {
	var ip string
	err := m.store.Upsert(ctx, subnetId, func(record *IPAMRecord) (bool, error) {
		// Only an absent record takes the caller's CIDR. A stored one keeps its
		// own, so a caller passing a stale subnet CIDR cannot re-base a pool
		// that already has addresses handed out of it.
		if record.CidrBlock == "" {
			record.SubnetId = subnetId
			record.CidrBlock = cidrBlock
		}

		next, err := m.nextAvailableIP(record)
		if err != nil {
			return false, err
		}

		record.Allocated = append(record.Allocated, IPEntry{IP: next, Purpose: purpose, OwnerID: ownerID})
		ip = next
		return true, nil
	})
	if err != nil {
		return "", err
	}

	slog.Info("IPAM allocated IP", "subnet", subnetId, "ip", ip, "purpose", purpose, "owner", ownerID)
	return ip, nil
}

// ClaimIP reserves a caller-specified IP in the subnet, for a pinned address
// request (e.g. RunInstances --private-ip-address). Validates the address is
// inside the subnet, not reserved/broadcast, and not already allocated, then
// uses the same CAS loop as AllocateIP so a concurrent claim of the same
// address always leaves exactly one winner.
func (m *IPAM) ClaimIP(ctx context.Context, subnetId, cidrBlock, purpose, ownerID, requestedIP string) (string, error) {
	addr, err := netip.ParseAddr(requestedIP)
	if err != nil {
		return "", fmt.Errorf("%w: %q is not a valid IP address", ErrIPOutOfRange, requestedIP)
	}

	prefix, err := netip.ParsePrefix(cidrBlock)
	if err != nil {
		return "", fmt.Errorf("parse CIDR %q: %w", cidrBlock, err)
	}

	if !isClaimableAddr(prefix, addr) {
		return "", fmt.Errorf("%w: %s is not a usable address in subnet %s", ErrIPOutOfRange, addr, cidrBlock)
	}

	ip := addr.String()

	err = m.store.Upsert(ctx, subnetId, func(record *IPAMRecord) (bool, error) {
		// Only an absent record takes the caller's CIDR, matching AllocateIP: a
		// stale subnet CIDR must not re-base a pool with addresses already out.
		if record.CidrBlock == "" {
			record.SubnetId = subnetId
			record.CidrBlock = cidrBlock
		}
		// Re-checked on every attempt rather than once up front, so a claim that
		// lost the race to the same address reports it in use instead of
		// appending a duplicate entry.
		for _, entry := range record.Allocated {
			if entry.IP == ip {
				return false, fmt.Errorf("%w: %s already allocated in subnet %s", ErrIPInUse, ip, subnetId)
			}
		}
		record.Allocated = append(record.Allocated, IPEntry{IP: ip, Purpose: purpose, OwnerID: ownerID})
		return true, nil
	})
	if err != nil {
		return "", err
	}

	slog.Info("IPAM claimed IP", "subnet", subnetId, "ip", ip, "purpose", purpose, "owner", ownerID)
	return ip, nil
}

// isClaimableAddr reports whether addr is a usable host address in prefix:
// inside the prefix, outside the reserved head (network + 3 AWS-reserved
// addresses), and not the broadcast (last) address.
func isClaimableAddr(prefix netip.Prefix, addr netip.Addr) bool {
	if !prefix.Contains(addr) {
		return false
	}

	head := prefix.Masked().Addr()
	for range 4 {
		if addr == head {
			return false
		}
		head = head.Next()
	}

	next := addr.Next()
	return next.IsValid() && prefix.Contains(next)
}

// ReleaseIP releases a previously allocated IP address back to the subnet pool.
func (m *IPAM) ReleaseIP(ctx context.Context, subnetId, ip string) error {
	err := m.store.Mutate(ctx, subnetId, func(record *IPAMRecord) (bool, error) {
		for i, entry := range record.Allocated {
			if entry.IP == ip {
				record.Allocated = append(record.Allocated[:i], record.Allocated[i+1:]...)
				return true, nil
			}
		}
		return false, fmt.Errorf("IP %s not allocated in subnet %s", ip, subnetId)
	})
	if errors.Is(err, kvstore.ErrNotFound) {
		return fmt.Errorf("no IPAM record for subnet %s", subnetId)
	}
	if err != nil {
		return err
	}

	slog.Info("IPAM released IP", "subnet", subnetId, "ip", ip)
	return nil
}

// AllocatedIPs returns the list of allocated IP entries for a subnet.
func (m *IPAM) AllocatedIPs(ctx context.Context, subnetId string) ([]IPEntry, error) {
	record, _, err := m.store.Get(ctx, subnetId)
	if err != nil {
		if errors.Is(err, kvstore.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return record.Allocated, nil
}

// nextAvailableIP finds the next available IP in the subnet, skipping reserved addresses.
func (m *IPAM) nextAvailableIP(record *IPAMRecord) (string, error) {
	prefix, err := netip.ParsePrefix(record.CidrBlock)
	if err != nil {
		return "", fmt.Errorf("parse CIDR %q: %w", record.CidrBlock, err)
	}

	allocated := make(map[string]bool, len(record.Allocated))
	for _, entry := range record.Allocated {
		allocated[entry.IP] = true
	}

	// Start at offset 4 (.0=network, .1=gateway, .2=DNS, .3=reserved). Masked()
	// normalises a CIDR written with host bits set, e.g. 10.0.1.7/24.
	addr := prefix.Masked().Addr()
	for range 4 {
		addr = addr.Next()
	}

	// Walk until the successor leaves the prefix, which reserves the broadcast
	// address. Subnets too small to hold the reserved head (/30 and narrower)
	// start already outside the prefix and fall straight through to exhausted.
	for ; prefix.Contains(addr.Next()); addr = addr.Next() {
		candidate := addr.String()
		if !allocated[candidate] {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("subnet %s exhausted, no IPs available", record.CidrBlock)
}
