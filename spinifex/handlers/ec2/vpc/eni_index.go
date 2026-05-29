package handlers_ec2_vpc

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
)

// eniByIPValue is the stored shape of a spinifex-network-eni-by-vpc-ip row. It
// carries the composite identity the IMDS handler needs to locate the ENI's
// source-of-truth record — the ENI ID plus its owning account, since the ENI
// bucket is keyed "{accountID}.{eniID}". Both fields are immutable for the
// ENI's lifetime, so this is identity, not staleness-prone denormalisation: the
// handler still reads the live ENIRecord + instance record for every mutable
// field (IPs, MAC, instance ID, profile ARN). Storing account_id lets the
// handler resolve the account in a single Get instead of scanning the ENI
// bucket's keys for the matching "{accountID}.{eniID}" suffix.
type eniByIPValue struct {
	ENIId     string `json:"eni_id"`
	AccountID string `json:"account_id"`
}

// ENIByIPIndex maintains the spinifex-network-eni-by-vpc-ip reverse index
// (vpcID/ip → eniID). It lets the IMDS handler resolve a request's
// datapath-attested source IP to an ENI in one KV read instead of an O(N) scan
// of the ENI bucket. The IP↔ENI binding is fixed for the ENI's lifetime, so
// the index is written on CreateNetworkInterface and deleted on
// DeleteNetworkInterface — those are the only two write sites.
type ENIByIPIndex struct {
	kv nats.KeyValue
}

// NewENIByIPIndex wraps an already-opened eni-by-vpc-ip KV bucket.
func NewENIByIPIndex(kv nats.KeyValue) *ENIByIPIndex { return &ENIByIPIndex{kv: kv} }

// eniByVPCIPKey builds the composite vpcID/ip key.
func eniByVPCIPKey(vpcID, ip string) string { return vpcID + "/" + ip }

// Put writes the vpcID/ip → {eniID, accountID} mapping, overwriting any prior
// entry. Called after the source-of-truth ENI record is persisted so that a
// failure here leaves IMDS returning safe 404s rather than phantom permissions.
func (i *ENIByIPIndex) Put(vpcID, ip, eniID, accountID string) error {
	data, err := json.Marshal(eniByIPValue{ENIId: eniID, AccountID: accountID})
	if err != nil {
		return fmt.Errorf("marshal eni-by-ip value: %w", err)
	}
	if _, err := i.kv.Put(eniByVPCIPKey(vpcID, ip), data); err != nil {
		return fmt.Errorf("put eni-by-ip index %s/%s: %w", vpcID, ip, err)
	}
	return nil
}

// Delete removes the vpcID/ip mapping. Idempotent: a missing key returns nil.
func (i *ENIByIPIndex) Delete(vpcID, ip string) error {
	if err := i.kv.Delete(eniByVPCIPKey(vpcID, ip)); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return fmt.Errorf("delete eni-by-ip index %s/%s: %w", vpcID, ip, err)
	}
	return nil
}
