package handlers_imds

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
)

// SubnetVethRecord is the persisted per-subnet IMDS plumbing record
// (KVBucketIMDSSubnetVeth, keyed by subnet ID): the signal that a subnet's IMDS
// localport is installed, replayed by each chassis's BindManager. It carries the
// LSP MAC and the subnet CIDR the host can't re-derive, plus the owning VPC for
// the subnet→VPC reverse lookup.
type SubnetVethRecord struct {
	SubnetID      string `json:"subnet_id"`
	ShortSubnetID string `json:"short_subnet_id"`
	VPCID         string `json:"vpc_id"`
	IMDSPortMAC   string `json:"imds_port_mac"`
	SubnetCIDR    string `json:"subnet_cidr"`
	CreatedAt     string `json:"created_at"`
}

// VethStore is the persistence surface for SubnetVethRecord rows in
// KVBucketIMDSSubnetVeth. Backed by a NATS JetStream KV bucket opened by
// InitBuckets in production; tests inject the handle via NewVethStore.
type VethStore struct {
	kv nats.KeyValue
}

// NewVethStore wraps an already-opened subnet-veth KV bucket.
func NewVethStore(kv nats.KeyValue) *VethStore { return &VethStore{kv: kv} }

// Get returns the record for subnetID, or (nil, nil) when absent.
func (s *VethStore) Get(subnetID string) (*SubnetVethRecord, error) {
	raw, err := s.kv.Get(subnetID)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get imds veth record %q: %w", subnetID, err)
	}
	var rec SubnetVethRecord
	if err := json.Unmarshal(raw.Value(), &rec); err != nil {
		return nil, fmt.Errorf("unmarshal imds veth record %q: %w", subnetID, err)
	}
	return &rec, nil
}

// Put writes the record keyed by its SubnetID, overwriting any prior entry.
func (s *VethStore) Put(rec SubnetVethRecord) error {
	if rec.SubnetID == "" {
		return errors.New("imds veth store put: subnet_id is empty")
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal imds veth record: %w", err)
	}
	if _, err := s.kv.Put(rec.SubnetID, data); err != nil {
		return fmt.Errorf("put imds veth record %q: %w", rec.SubnetID, err)
	}
	return nil
}

// Delete removes the record. Idempotent: a missing key returns nil.
func (s *VethStore) Delete(subnetID string) error {
	if err := s.kv.Delete(subnetID); err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil
		}
		return fmt.Errorf("delete imds veth record %q: %w", subnetID, err)
	}
	return nil
}
