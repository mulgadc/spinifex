package handlers_imds

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"
)

// kvBucketENIs is the ENI source-of-truth bucket (handlers_ec2_vpc.KVBucketENIs),
// duplicated as a literal rather than imported because importing handlers/ec2/vpc
// back would close an import cycle (the veth_store record type does likewise).
const kvBucketENIs = "spinifex-vpc-enis"

// kvBucketSecurityGroups is the security-group source-of-truth bucket
// (handlers_ec2_vpc.KVBucketSecurityGroups). Duplicated as a literal for the
// same import-cycle reason as kvBucketENIs.
const kvBucketSecurityGroups = "spinifex-vpc-security-groups"

// eniFacts is the subset of an ENIRecord the IMDS metadata surface serves
// directly, plus the account ID recovered from the ENI bucket key. Everything
// here is read live off the ENI record, so a change (e.g. a re-associated EIP)
// is reflected on the next request.
type eniFacts struct {
	eniID            string
	accountID        string
	instanceID       string
	vpcID            string
	subnetID         string
	privateIP        string
	publicIP         string
	mac              string
	availabilityZone string
	securityGroupIDs []string
}

// instanceFacts carries the four metadata fields not present on the ENI record.
// They live in the daemon's in-memory instance manager, so they are fetched via
// the account-scoped DescribeInstances fan-out behind instanceLookup.
type instanceFacts struct {
	instanceType          string
	imageID               string
	iamInstanceProfileArn string
	userData              []byte
}

// instanceLookup resolves the instance-only metadata fields by instance ID. It
// is an interface so the metadata handlers stay unit-testable without a live
// NATS cluster; the production implementation (natsInstanceLookup) fans out
// DescribeInstances + DescribeInstanceAttribute.
type instanceLookup interface {
	describe(accountID, instanceID string) (*instanceFacts, error)
}

// eniIndexValue mirrors the stored shape of a spinifex-network-eni-by-vpc-ip
// row (handlers_ec2_vpc.eniByIPValue). Duplicated for the same cycle reason as
// kvBucketENIs. Both fields are the ENI's immutable identity; everything mutable
// is read live off the ENI record.
type eniIndexValue struct {
	ENIId     string `json:"eni_id"`
	AccountID string `json:"account_id"`
}

// sgNameRecord is the slice of handlers_ec2_vpc.SecurityGroupRecord the metadata
// surface needs: the human-readable group name AWS serves at
// /latest/meta-data/security-groups (not the sg-* ID).
type sgNameRecord struct {
	GroupName string `json:"group_name"`
}

// eniRecord is the subset of handlers_ec2_vpc.ENIRecord the resolver reads. The
// full record is owned by the ENI controller; only these fields feed IMDS.
type eniRecord struct {
	NetworkInterfaceId string   `json:"network_interface_id"`
	SubnetId           string   `json:"subnet_id"`
	VpcId              string   `json:"vpc_id"`
	AvailabilityZone   string   `json:"availability_zone"`
	PrivateIpAddress   string   `json:"private_ip_address"`
	MacAddress         string   `json:"mac_address"`
	InstanceId         string   `json:"instance_id,omitempty"`
	PublicIpAddress    string   `json:"public_ip_address,omitempty"`
	SecurityGroupIds   []string `json:"security_group_ids,omitempty"`
}

// metadataResolver maps a datapath-attested (vpcID, srcIP) to the ENI + instance
// facts the metadata surface serves. The chain is:
//
//	(vpcID, ip) → {eniID, accountID}   via the eni-by-vpc-ip reverse index
//	(account, eniID) → ENIRecord       via a direct KV Get
//	(account, instanceID) → facts      via the account-scoped DescribeInstances fan-out
//
// The index carries the ENI's immutable identity so the account resolves in one
// Get; every mutable field is read live off the record, so there's no staleness.
type metadataResolver struct {
	index  nats.KeyValue // spinifex-network-eni-by-vpc-ip
	eniKV  nats.KeyValue // spinifex-vpc-enis
	sgKV   nats.KeyValue // spinifex-vpc-security-groups (nil-safe: degrades to IDs)
	lookup instanceLookup
}

// resolveENI returns the ENI facts for a request's (vpcID, srcIP), or (nil, nil)
// when no mapping exists — the caller maps a miss to 404, matching AWS's
// "eventually available during boot" posture. A non-nil error is reserved for
// genuine backend failures.
func (r *metadataResolver) resolveENI(vpcID, srcIP string) (*eniFacts, error) {
	entry, err := r.index.Get(vpcID + "/" + srcIP)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get eni-by-ip index %s/%s: %w", vpcID, srcIP, err)
	}

	var idx eniIndexValue
	if err := json.Unmarshal(entry.Value(), &idx); err != nil {
		return nil, fmt.Errorf("unmarshal eni-by-ip index %s/%s: %w", vpcID, srcIP, err)
	}
	if idx.ENIId == "" || idx.AccountID == "" {
		return nil, nil
	}

	raw, err := r.eniKV.Get(idx.AccountID + "." + idx.ENIId)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get eni record %s.%s: %w", idx.AccountID, idx.ENIId, err)
	}

	var rec eniRecord
	if err := json.Unmarshal(raw.Value(), &rec); err != nil {
		return nil, fmt.Errorf("unmarshal eni record %s: %w", idx.ENIId, err)
	}

	return &eniFacts{
		eniID:            rec.NetworkInterfaceId,
		accountID:        idx.AccountID,
		instanceID:       rec.InstanceId,
		vpcID:            rec.VpcId,
		subnetID:         rec.SubnetId,
		privateIP:        rec.PrivateIpAddress,
		publicIP:         rec.PublicIpAddress,
		mac:              rec.MacAddress,
		availabilityZone: rec.AvailabilityZone,
		securityGroupIDs: rec.SecurityGroupIds,
	}, nil
}

// resolveInstance fetches the instance-only metadata fields for an ENI's
// attached instance. Returns (nil, nil) when the ENI has no attached instance.
func (r *metadataResolver) resolveInstance(eni *eniFacts) (*instanceFacts, error) {
	if eni.instanceID == "" {
		return nil, nil
	}
	return r.lookup.describe(eni.accountID, eni.instanceID)
}

// resolveSGNames maps security-group IDs to the group names AWS serves at
// /latest/meta-data/security-groups. Best-effort and order-preserving: a missing
// record or nil sgKV falls back to the raw ID rather than failing the request.
func (r *metadataResolver) resolveSGNames(accountID string, sgIDs []string) []string {
	names := make([]string, len(sgIDs))
	for i, id := range sgIDs {
		names[i] = id
		if r.sgKV == nil {
			continue
		}
		raw, err := r.sgKV.Get(accountID + "." + id)
		if err != nil {
			if !errors.Is(err, nats.ErrKeyNotFound) {
				slog.Warn("IMDS: security-group name lookup failed", "account_id", accountID, "sg_id", id, "err", err)
			}
			continue
		}
		var rec sgNameRecord
		if err := json.Unmarshal(raw.Value(), &rec); err != nil {
			slog.Warn("IMDS: security-group unmarshal failed", "sg_id", id, "err", err)
			continue
		}
		if rec.GroupName != "" {
			names[i] = rec.GroupName
		}
	}
	return names
}
