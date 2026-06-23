package handlers_imds

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// kvBucketENIs and kvBucketSecurityGroups are duplicated as literals to avoid an import cycle.
const kvBucketENIs = "spinifex-vpc-enis"
const kvBucketSecurityGroups = "spinifex-vpc-security-groups"

// eniFacts is the ENI fields served by the IMDS metadata surface, read live from the ENI record.
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

// instanceFacts carries metadata fields not present on the ENI record, fetched via instanceLookup.
type instanceFacts struct {
	instanceType          string
	imageID               string
	iamInstanceProfileArn string
	keyName               string
	architecture          string
	reservationID         string
	amiLaunchIndex        int64
	pendingTime           time.Time
	userData              []byte
}

// instanceLookup resolves instance-only metadata fields by instance ID.
type instanceLookup interface {
	describe(accountID, instanceID string) (*instanceFacts, error)
}

// sgNameRecord holds the human-readable group name for the /security-groups path.
type sgNameRecord struct {
	GroupName string `json:"group_name"`
}

// eniRecord is the ENI record subset the resolver reads from the ENI bucket.
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

// metadataResolver resolves a tap's ENI ID to ENI + instance facts.
// Resolution chain: eniID → ENIRecord (account recovered from the bucket key) → instanceFacts.
type metadataResolver struct {
	eniKV  nats.KeyValue // spinifex-vpc-enis
	sgKV   nats.KeyValue // spinifex-vpc-security-groups (nil-safe: degrades to IDs)
	lookup instanceLookup
}

var _ eniResolver = (*metadataResolver)(nil)

// resolveENIByID returns ENI facts for an ENI located by its ID alone — the per-tap
// identity path, where the tap maps one-to-one to an ENI so no (vpcID, srcIP) lookup
// is needed. The owning account is recovered by suffix-scanning the ENI bucket
// (keyed "{accountID}.{eniID}"). Returns (nil, nil) on miss.
func (r *metadataResolver) resolveENIByID(eniID string) (*eniFacts, error) {
	if eniID == "" {
		return nil, nil
	}
	accountID, raw, err := r.findENIByID(eniID)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}

	var rec eniRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("unmarshal eni record %s: %w", eniID, err)
	}
	return eniFactsFromRecord(accountID, &rec), nil
}

// findENIByID scans the ENI bucket for the record whose key ends in ".{eniID}",
// returning the owning account and the raw record bytes, or ("", nil, nil) on
// miss. ENI IDs are globally unique, so at most one key matches.
func (r *metadataResolver) findENIByID(eniID string) (string, []byte, error) {
	keys, err := r.eniKV.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return "", nil, nil
		}
		return "", nil, fmt.Errorf("list eni bucket: %w", err)
	}

	suffix := "." + eniID
	for _, key := range keys {
		if !strings.HasSuffix(key, suffix) {
			continue
		}
		entry, err := r.eniKV.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				continue // raced with a concurrent delete
			}
			return "", nil, fmt.Errorf("get eni record %s: %w", key, err)
		}
		return strings.TrimSuffix(key, suffix), entry.Value(), nil
	}
	return "", nil, nil
}

// eniFactsFromRecord projects an ENI record plus its owning account into the
// fact subset the metadata surface serves.
func eniFactsFromRecord(accountID string, rec *eniRecord) *eniFacts {
	return &eniFacts{
		eniID:            rec.NetworkInterfaceId,
		accountID:        accountID,
		instanceID:       rec.InstanceId,
		vpcID:            rec.VpcId,
		subnetID:         rec.SubnetId,
		privateIP:        rec.PrivateIpAddress,
		publicIP:         rec.PublicIpAddress,
		mac:              rec.MacAddress,
		availabilityZone: rec.AvailabilityZone,
		securityGroupIDs: rec.SecurityGroupIds,
	}
}

// resolveInstance fetches instance-only fields for an ENI's attached instance, or (nil, nil) if unattached.
func (r *metadataResolver) resolveInstance(eni *eniFacts) (*instanceFacts, error) {
	if eni.instanceID == "" {
		return nil, nil
	}
	return r.lookup.describe(eni.accountID, eni.instanceID)
}

// resolveSGNames maps SG IDs to group names for /security-groups. Best-effort; falls back to IDs.
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
