package handlers_ec2_vpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	KVBucketIPAM        = "spinifex-vpc-ipam"
	KVBucketIPAMVersion = 2
)

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
	kv jetstream.KeyValue
}

// NewIPAM creates a new IPAM instance backed by NATS JetStream KV.
func NewIPAM(ctx context.Context, js jetstream.JetStream) (*IPAM, error) {
	kv, err := kvutil.GetOrCreateBucket(ctx, js, KVBucketIPAM, 5)
	if err != nil {
		return nil, fmt.Errorf("failed to create IPAM KV bucket: %w", err)
	}
	if err := migrate.DefaultRegistry.RunKVWithJetStream(ctx, KVBucketIPAM, kv, js, KVBucketIPAMVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketIPAM, err)
	}
	return &IPAM{kv: kv}, nil
}

// NewIPAMWithKV creates an IPAM with an existing KV bucket (for testing).
func NewIPAMWithKV(kv jetstream.KeyValue) *IPAM {
	return &IPAM{kv: kv}
}

// AllocateIP allocates an IP from the subnet, reserving the first 4 and last
// addresses per AWS convention. Uses CAS for conflict-free multi-node allocation.
func (m *IPAM) AllocateIP(ctx context.Context, subnetId, cidrBlock, purpose, ownerID string) (string, error) {
	for attempt := range 5 {
		record, revision, err := m.getRecord(ctx, subnetId)
		if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
			return "", fmt.Errorf("get IPAM record: %w", err)
		}

		if record == nil {
			record = &IPAMRecord{
				SubnetId:  subnetId,
				CidrBlock: cidrBlock,
			}
		}

		// Find next available IP
		ip, err := m.nextAvailableIP(record)
		if err != nil {
			return "", err
		}

		record.Allocated = append(record.Allocated, IPEntry{IP: ip, Purpose: purpose, OwnerID: ownerID})

		// CAS write
		data, err := json.Marshal(record)
		if err != nil {
			return "", fmt.Errorf("marshal IPAM record: %w", err)
		}

		if revision == 0 {
			_, err = m.kv.Create(ctx, subnetId, data)
		} else {
			_, err = m.kv.Update(ctx, subnetId, data, revision)
		}

		if err != nil {
			slog.Debug("IPAM CAS conflict, retrying", "subnet", subnetId, "attempt", attempt)
			continue // CAS conflict, retry
		}

		slog.Info("IPAM allocated IP", "subnet", subnetId, "ip", ip, "purpose", purpose, "owner", ownerID)
		return ip, nil
	}

	return "", fmt.Errorf("IPAM allocation failed after CAS retries for subnet %s", subnetId)
}

// ReleaseIP releases a previously allocated IP address back to the subnet pool.
func (m *IPAM) ReleaseIP(ctx context.Context, subnetId, ip string) error {
	for attempt := range 5 {
		record, revision, err := m.getRecord(ctx, subnetId)
		if err != nil {
			return fmt.Errorf("get IPAM record for release: %w", err)
		}
		if record == nil {
			return fmt.Errorf("no IPAM record for subnet %s", subnetId)
		}

		found := false
		for i, entry := range record.Allocated {
			if entry.IP == ip {
				record.Allocated = append(record.Allocated[:i], record.Allocated[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("IP %s not allocated in subnet %s", ip, subnetId)
		}

		data, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("marshal IPAM record: %w", err)
		}

		if _, err := m.kv.Update(ctx, subnetId, data, revision); err != nil {
			slog.Debug("IPAM release CAS conflict, retrying", "subnet", subnetId, "attempt", attempt)
			continue
		}

		slog.Info("IPAM released IP", "subnet", subnetId, "ip", ip)
		return nil
	}

	return fmt.Errorf("IPAM release failed after CAS retries for subnet %s", subnetId)
}

// AllocatedIPs returns the list of allocated IP entries for a subnet.
func (m *IPAM) AllocatedIPs(ctx context.Context, subnetId string) ([]IPEntry, error) {
	record, _, err := m.getRecord(ctx, subnetId)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if record == nil {
		return nil, nil
	}
	return record.Allocated, nil
}

func (m *IPAM) getRecord(ctx context.Context, subnetId string) (*IPAMRecord, uint64, error) {
	entry, err := m.kv.Get(ctx, subnetId)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, 0, jetstream.ErrKeyNotFound
		}
		return nil, 0, err
	}

	var record IPAMRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, 0, fmt.Errorf("unmarshal IPAM record: %w", err)
	}

	return &record, entry.Revision(), nil
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
