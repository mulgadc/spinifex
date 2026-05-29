package handlers_imds

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
)

// VPCVethRecord is the persisted per-VPC IMDS plumbing record stored in
// KVBucketIMDSVPCVeth, keyed by VPC ID. It is the canonical signal that a VPC
// has its IMDS OVN topology installed; every chassis's BindManager replays the
// record to materialise the local host veth + listener. The record carries
// only what the host side cannot re-derive from the VPC ID alone — the LSP MAC
// and the LRP /30 — since the veth and OVN object names are all deterministic
// functions of the VPC ID.
type VPCVethRecord struct {
	VPCID       string `json:"vpc_id"`
	ShortVPCID  string `json:"short_vpc_id"`
	IMDSPortMAC string `json:"imds_port_mac"`
	LRPNetwork  string `json:"lrp_network"`
	CreatedAt   string `json:"created_at"`
}

// VethStore is the persistence surface for VPCVethRecord rows in
// KVBucketIMDSVPCVeth. Backed by a NATS JetStream KV bucket opened by
// InitBuckets in production; tests inject the handle via NewVethStore.
type VethStore struct {
	kv nats.KeyValue
}

// NewVethStore wraps an already-opened vpc-veth KV bucket.
func NewVethStore(kv nats.KeyValue) *VethStore { return &VethStore{kv: kv} }

// Get returns the record for vpcID, or (nil, nil) when absent.
func (s *VethStore) Get(vpcID string) (*VPCVethRecord, error) {
	raw, err := s.kv.Get(vpcID)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get imds veth record %q: %w", vpcID, err)
	}
	var rec VPCVethRecord
	if err := json.Unmarshal(raw.Value(), &rec); err != nil {
		return nil, fmt.Errorf("unmarshal imds veth record %q: %w", vpcID, err)
	}
	return &rec, nil
}

// Put writes the record keyed by its VPCID, overwriting any prior entry.
func (s *VethStore) Put(rec VPCVethRecord) error {
	if rec.VPCID == "" {
		return errors.New("imds veth store put: vpc_id is empty")
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal imds veth record: %w", err)
	}
	if _, err := s.kv.Put(rec.VPCID, data); err != nil {
		return fmt.Errorf("put imds veth record %q: %w", rec.VPCID, err)
	}
	return nil
}

// Delete removes the record. Idempotent: a missing key returns nil.
func (s *VethStore) Delete(vpcID string) error {
	if err := s.kv.Delete(vpcID); err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil
		}
		return fmt.Errorf("delete imds veth record %q: %w", vpcID, err)
	}
	return nil
}
