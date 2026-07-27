package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

// ipamBucket / eniBucket mirror handlers/ec2/vpc.KVBucketIPAM and
// .KVBucketENIs. Duplicated here so the migrate package stays handler-free.
const (
	ipamBucket = "spinifex-vpc-ipam"
	eniBucket  = "spinifex-vpc-enis"
)

const ipamMigrationMaxRetries = 3

// ipEntryV2 mirrors handlers/ec2/vpc.IPEntry (frozen snapshot).
type ipEntryV2 struct {
	IP      string `json:"ip"`
	Purpose string `json:"purpose"`
	OwnerID string `json:"owner_id,omitempty"`
}

// ipamRecordV1V2 is a union of v1 ([]string) and v2 ([]IPEntry) schemas.
// Allocated is left as raw JSON so we can detect which schema is on-disk
// and re-marshal as IPEntry.
type ipamRecordV1V2 struct {
	SubnetId  string          `json:"subnet_id"`
	CidrBlock string          `json:"cidr_block"`
	Allocated json.RawMessage `json:"allocated"`
}

// ipamRecordV2 is the post-migration shape.
type ipamRecordV2 struct {
	SubnetId  string      `json:"subnet_id"`
	CidrBlock string      `json:"cidr_block"`
	Allocated []ipEntryV2 `json:"allocated"`
}

// eniRecordSnapshot is the minimum ENIRecord shape needed to attribute IPAM
// entries to their owning ENI. Frozen copy keeps the migration self-contained.
type eniRecordSnapshot struct {
	NetworkInterfaceId string `json:"network_interface_id"`
	SubnetId           string `json:"subnet_id"`
	PrivateIpAddress   string `json:"private_ip_address"`
}

func init() {
	DefaultRegistry.RegisterKV(ipamBucket, KVMigration{
		FromVersion: 1,
		ToVersion:   2,
		Description: "convert subnet IPAM Allocated []string → []IPEntry; backfill Purpose+OwnerID from ENIs",
		Run: func(ctx context.Context, kvc KVContext) error {
			ownerIndex, err := buildIPAMOwnerIndex(ctx, kvc.JetStream)
			if err != nil {
				return fmt.Errorf("build owner index: %w", err)
			}

			keys, err := kvc.KV.Keys(ctx)
			if err != nil {
				if errors.Is(err, jetstream.ErrNoKeysFound) {
					return nil
				}
				return fmt.Errorf("list keys: %w", err)
			}

			for _, key := range keys {
				if key == utils.VersionKey {
					continue
				}
				if err := convertIPAMRecord(ctx, kvc, key, ownerIndex); err != nil {
					return err
				}
			}
			return nil
		},
	})
}

// buildIPAMOwnerIndex returns map[subnetID]map[ip]eniID by walking the ENI
// bucket. Returns an empty index if the ENI bucket is missing
// (fresh-install case) so the migration still runs cleanly.
func buildIPAMOwnerIndex(ctx context.Context, js jetstream.JetStream) (map[string]map[string]string, error) {
	if js == nil {
		// Called via the bare RunKV path (e.g. in tests with no JS). Skip
		// owner attribution — all entries become Purpose=unknown so the
		// schema-only conversion still succeeds.
		return map[string]map[string]string{}, nil
	}
	eniKV, err := js.KeyValue(ctx, eniBucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return map[string]map[string]string{}, nil
		}
		return nil, fmt.Errorf("open ENI bucket: %w", err)
	}

	keys, err := eniKV.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return map[string]map[string]string{}, nil
		}
		return nil, fmt.Errorf("list ENI keys: %w", err)
	}

	index := make(map[string]map[string]string)
	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		entry, err := eniKV.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return nil, fmt.Errorf("get ENI %s: %w", key, err)
		}
		var eni eniRecordSnapshot
		if err := json.Unmarshal(entry.Value(), &eni); err != nil {
			continue
		}
		if eni.SubnetId == "" || eni.PrivateIpAddress == "" {
			continue
		}
		if _, ok := index[eni.SubnetId]; !ok {
			index[eni.SubnetId] = make(map[string]string)
		}
		index[eni.SubnetId][eni.PrivateIpAddress] = eni.NetworkInterfaceId
	}
	return index, nil
}

func convertIPAMRecord(ctx context.Context, kvc KVContext, key string, ownerIndex map[string]map[string]string) error {
	var lastErr error
	for attempt := range ipamMigrationMaxRetries {
		entry, err := kvc.KV.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				return nil
			}
			return fmt.Errorf("get %s: %w", key, err)
		}

		var raw ipamRecordV1V2
		if err := json.Unmarshal(entry.Value(), &raw); err != nil {
			return fmt.Errorf("unmarshal %s: %w", key, err)
		}

		// Detect schema: v2 records start with `[{`, v1 with `["`.
		// Idempotent skip when already migrated.
		if v2Entries, ok := tryDecodeV2(raw.Allocated); ok {
			_ = v2Entries
			return nil
		}

		var legacy []string
		if len(raw.Allocated) > 0 {
			if err := json.Unmarshal(raw.Allocated, &legacy); err != nil {
				return fmt.Errorf("unmarshal legacy []string for %s: %w", key, err)
			}
		}

		ownerByIP := ownerIndex[raw.SubnetId]
		converted := make([]ipEntryV2, 0, len(legacy))
		for _, ip := range legacy {
			ownerID, found := ownerByIP[ip]
			purpose := "eni-primary"
			if !found {
				ownerID = ""
				purpose = "unknown"
				kvc.Logger.Warn("internal IPAM v1→v2 migration: no owning ENI, tagging unknown",
					"subnet", raw.SubnetId, "ip", ip)
			}
			converted = append(converted, ipEntryV2{IP: ip, Purpose: purpose, OwnerID: ownerID})
		}

		newRecord := ipamRecordV2{
			SubnetId:  raw.SubnetId,
			CidrBlock: raw.CidrBlock,
			Allocated: converted,
		}
		data, err := json.Marshal(newRecord)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", key, err)
		}
		if _, err := kvc.KV.Update(ctx, key, data, entry.Revision()); err != nil {
			if errors.Is(err, jetstream.ErrKeyExists) {
				kvc.Logger.Warn("internal IPAM migration CAS conflict, retrying", "key", key, "attempt", attempt+1)
				lastErr = err
				continue
			}
			return fmt.Errorf("update %s: %w", key, err)
		}
		return nil
	}
	return fmt.Errorf("convert IPAM record %s: exceeded %d CAS retries: %w", key, ipamMigrationMaxRetries, lastErr)
}

// tryDecodeV2 returns true when raw decodes as []ipEntryV2 with at least one
// entry whose IP field is set — the cheapest reliable v2 detector. Empty
// arrays decode as either schema; we treat them as already migrated.
func tryDecodeV2(raw json.RawMessage) ([]ipEntryV2, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	var entries []ipEntryV2
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, false
	}
	for _, e := range entries {
		if e.IP != "" {
			return entries, true
		}
	}
	// Zero-length array — already v2-shaped.
	if len(entries) == 0 {
		return entries, true
	}
	return nil, false
}
