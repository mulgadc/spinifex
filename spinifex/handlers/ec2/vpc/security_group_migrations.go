package handlers_ec2_vpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// migrateAssignRuleIDsMaxRetries bounds CAS retry on revision conflict during
// the v1→v2 backfill. Spinifex deploys are full-downtime so concurrent writers
// should not exist; the retry is purely defensive against an overlapping
// daemon start during a rolling restart.
const migrateAssignRuleIDsMaxRetries = 3

func init() {
	migrate.DefaultRegistry.RegisterKV(KVBucketSecurityGroups, migrate.KVMigration{
		FromVersion: 1,
		ToVersion:   2,
		Description: "assign sgr- IDs to security group rules",
		Run:         migrateAssignRuleIDs,
	})
}

// migrateAssignRuleIDs walks every SecurityGroupRecord in the bucket and
// assigns a fresh sgr- ID to any rule whose RuleId is empty. Records whose
// rules already have IDs are skipped to keep the migration idempotent and
// avoid spurious revision bumps.
func migrateAssignRuleIDs(ctx migrate.KVContext) error {
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
		if strings.HasPrefix(key, "_") {
			continue
		}
		if err := backfillRuleIDsForKey(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func backfillRuleIDsForKey(ctx migrate.KVContext, key string) error {
	for attempt := range migrateAssignRuleIDsMaxRetries {
		entry, err := ctx.KV.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				return nil
			}
			return fmt.Errorf("get %s: %w", key, err)
		}

		var record SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			return fmt.Errorf("unmarshal %s: %w", key, err)
		}

		changed := false
		for i := range record.IngressRules {
			if record.IngressRules[i].RuleId == "" {
				record.IngressRules[i].RuleId = utils.GenerateResourceID("sgr")
				changed = true
			}
		}
		for i := range record.EgressRules {
			if record.EgressRules[i].RuleId == "" {
				record.EgressRules[i].RuleId = utils.GenerateResourceID("sgr")
				changed = true
			}
		}

		if !changed {
			return nil
		}

		data, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", key, err)
		}
		if _, err := ctx.KV.Update(key, data, entry.Revision()); err != nil {
			if errors.Is(err, nats.ErrKeyExists) || isCASConflict(err) {
				ctx.Logger.Warn("SG migration CAS conflict, retrying", "key", key, "attempt", attempt+1)
				continue
			}
			return fmt.Errorf("update %s: %w", key, err)
		}
		return nil
	}
	return fmt.Errorf("backfill rule IDs for %s: exceeded %d CAS retries", key, migrateAssignRuleIDsMaxRetries)
}

// isCASConflict detects the nats.go "wrong last sequence" error that
// nats.KeyValue.Update returns on a revision mismatch. The library surfaces
// the underlying JetStream APIError text rather than a sentinel; matching on
// substring keeps the retry behaviour decoupled from that internal shape.
func isCASConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "wrong last sequence") || strings.Contains(msg, "revision mismatch")
}
