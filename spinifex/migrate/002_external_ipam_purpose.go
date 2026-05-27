package migrate

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// extIPAMBucket mirrors handlers/ec2/vpc.KVBucketExternalIPAM. Duplicated to
// keep the migrate package free of any handler dependency; if the bucket name
// changes, both constants must move together.
const extIPAMBucket = "spinifex-external-ipam"

const extIPAMMaxRetries = 3

// extLegacyTypeToPurpose maps the v1 ExternalIPAllocation.Type values onto
// the v2 Purpose enum. Frozen copy of vpc.LegacyExternalTypeToPurpose to
// keep this migration self-contained.
var extLegacyTypeToPurpose = map[string]string{
	"gateway":     "igw-lrp",
	"auto_assign": "eni-public",
	"elastic_ip":  "eip",
}

// extIPAllocV1V2 is a union of the v1 and v2 ExternalIPAllocation schemas.
// The migration reads the legacy Type field, writes the new Purpose field,
// and clears Type so the on-disk record matches the v2 struct exactly.
type extIPAllocV1V2 struct {
	Type         string `json:"type,omitempty"`
	Purpose      string `json:"purpose,omitempty"`
	AllocationID string `json:"allocation_id,omitempty"`
	Association  string `json:"association,omitempty"`
	ENIId        string `json:"eni_id,omitempty"`
	InstanceId   string `json:"instance_id,omitempty"`
	Note         string `json:"note,omitempty"`
}

type extIPAMRecordV1V2 struct {
	PoolName        string                    `json:"pool_name"`
	RangeStart      string                    `json:"range_start"`
	RangeEnd        string                    `json:"range_end"`
	Gateway         string                    `json:"gateway"`
	GatewayIP       string                    `json:"gateway_ip"`
	PrefixLen       int                       `json:"prefix_len"`
	Region          string                    `json:"region,omitempty"`
	AZ              string                    `json:"az,omitempty"`
	GwLrpRangeStart string                    `json:"gw_lrp_range_start,omitempty"`
	GwLrpRangeEnd   string                    `json:"gw_lrp_range_end,omitempty"`
	Allocated       map[string]extIPAllocV1V2 `json:"allocated"`
}

func init() {
	DefaultRegistry.RegisterKV(extIPAMBucket, KVMigration{
		FromVersion: 1,
		ToVersion:   2,
		Description: "rename ExternalIPAllocation.Type → .Purpose with enum mapping",
		Run: func(ctx KVContext) error {
			keys, err := ctx.KV.Keys()
			if err != nil {
				if errors.Is(err, nats.ErrNoKeysFound) {
					return nil
				}
				return fmt.Errorf("list keys: %w", err)
			}

			for _, key := range keys {
				if key == utils.VersionKey {
					continue
				}
				if err := renameExternalIPAMType(ctx, key); err != nil {
					return err
				}
			}
			return nil
		},
	})
}

func renameExternalIPAMType(ctx KVContext, key string) error {
	var lastErr error
	for attempt := range extIPAMMaxRetries {
		entry, err := ctx.KV.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				return nil
			}
			return fmt.Errorf("get %s: %w", key, err)
		}

		var record extIPAMRecordV1V2
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			return fmt.Errorf("unmarshal %s: %w", key, err)
		}

		changed := false
		for ip, alloc := range record.Allocated {
			if alloc.Purpose != "" {
				continue
			}
			purpose := extLegacyTypeToPurpose[alloc.Type]
			if purpose == "" {
				ctx.Logger.Warn("external IPAM v1→v2 migration: unknown legacy type, tagging unknown",
					"pool", key, "ip", ip, "legacy_type", alloc.Type)
				purpose = "unknown"
			}
			alloc.Purpose = purpose
			alloc.Type = ""
			record.Allocated[ip] = alloc
			changed = true
		}

		if !changed {
			return nil
		}

		data, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", key, err)
		}
		if _, err := ctx.KV.Update(key, data, entry.Revision()); err != nil {
			if errors.Is(err, nats.ErrKeyExists) {
				ctx.Logger.Warn("external IPAM migration CAS conflict, retrying", "key", key, "attempt", attempt+1)
				lastErr = err
				continue
			}
			return fmt.Errorf("update %s: %w", key, err)
		}
		return nil
	}
	return fmt.Errorf("rename Type→Purpose for %s: exceeded %d CAS retries: %w", key, extIPAMMaxRetries, lastErr)
}
