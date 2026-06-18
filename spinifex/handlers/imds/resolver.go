package handlers_imds

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// eniIndexValue mirrors the eni-by-vpc-ip row shape, duplicated to avoid an import cycle.
type eniIndexValue struct {
	ENIId     string `json:"eni_id"`
	AccountID string `json:"account_id"`
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

// metadataResolver maps a datapath-attested (vpcID, srcIP) to ENI + instance facts.
// Resolution chain: (vpcID,ip)→eniID via reverse index → ENIRecord → instanceFacts.
type metadataResolver struct {
	index  nats.KeyValue // spinifex-network-eni-by-vpc-ip
	eniKV  nats.KeyValue // spinifex-vpc-enis
	sgKV   nats.KeyValue // spinifex-vpc-security-groups (nil-safe: degrades to IDs)
	lookup instanceLookup
}

var _ eniResolver = (*metadataResolver)(nil)

// resolveENI returns ENI facts for (vpcID, srcIP), or (nil, nil) on miss.
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
