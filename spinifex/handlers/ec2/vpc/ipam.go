package handlers_ec2_vpc

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
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
	kv nats.KeyValue
}

// NewIPAM creates a new IPAM instance backed by NATS JetStream KV.
func NewIPAM(js nats.JetStreamContext) (*IPAM, error) {
	kv, err := utils.GetOrCreateKVBucket(js, KVBucketIPAM, 5)
	if err != nil {
		return nil, fmt.Errorf("failed to create IPAM KV bucket: %w", err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketIPAM, kv, KVBucketIPAMVersion, js); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketIPAM, err)
	}
	return &IPAM{kv: kv}, nil
}

// NewIPAMWithKV creates an IPAM with an existing KV bucket (for testing).
func NewIPAMWithKV(kv nats.KeyValue) *IPAM {
	return &IPAM{kv: kv}
}

// AllocateIP allocates an IP from the subnet, reserving the first 4 and last
// addresses per AWS convention. Uses CAS for conflict-free multi-node allocation.
func (m *IPAM) AllocateIP(subnetId, cidrBlock, purpose, ownerID string) (string, error) {
	for attempt := range 5 {
		record, revision, err := m.getRecord(subnetId)
		if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
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
			_, err = m.kv.Create(subnetId, data)
		} else {
			_, err = m.kv.Update(subnetId, data, revision)
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
func (m *IPAM) ReleaseIP(subnetId, ip string) error {
	for attempt := range 5 {
		record, revision, err := m.getRecord(subnetId)
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

		if _, err := m.kv.Update(subnetId, data, revision); err != nil {
			slog.Debug("IPAM release CAS conflict, retrying", "subnet", subnetId, "attempt", attempt)
			continue
		}

		slog.Info("IPAM released IP", "subnet", subnetId, "ip", ip)
		return nil
	}

	return fmt.Errorf("IPAM release failed after CAS retries for subnet %s", subnetId)
}

// AllocatedIPs returns the list of allocated IP entries for a subnet.
func (m *IPAM) AllocatedIPs(subnetId string) ([]IPEntry, error) {
	record, _, err := m.getRecord(subnetId)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if record == nil {
		return nil, nil
	}
	return record.Allocated, nil
}

func (m *IPAM) getRecord(subnetId string) (*IPAMRecord, uint64, error) {
	entry, err := m.kv.Get(subnetId)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, 0, nats.ErrKeyNotFound
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
	_, ipNet, err := net.ParseCIDR(record.CidrBlock)
	if err != nil {
		return "", fmt.Errorf("parse CIDR %q: %w", record.CidrBlock, err)
	}

	allocated := make(map[string]bool, len(record.Allocated))
	for _, entry := range record.Allocated {
		allocated[entry.IP] = true
	}

	ones, bits := ipNet.Mask.Size()
	hostBits := bits - ones           // 0-32 for IPv4 CIDRs
	totalIPs := uint64(1) << hostBits // fits uint64 (max 2^32)

	// Start at offset 4 (.0=network, .1=gateway, .2=DNS, .3=reserved)
	networkIP := ipToInt(ipNet.IP)

	for offset := uint64(4); offset < totalIPs-1; offset++ { // -1 for broadcast
		candidate := intToIP(networkIP + uint32(offset)).String()
		if !allocated[candidate] {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("subnet %s exhausted, no IPs available", record.CidrBlock)
}

// ipToInt converts an IPv4 net.IP to its uint32 representation.
func ipToInt(ip net.IP) uint32 {
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	return binary.BigEndian.Uint32(ip)
}

// intToIP converts a uint32 back to an IPv4 net.IP.
func intToIP(n uint32) net.IP {
	ip := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(ip, n)
	return ip
}
