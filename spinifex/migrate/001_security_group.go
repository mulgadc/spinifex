package migrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// sgBucket mirrors handlers/ec2/vpc.KVBucketSecurityGroups. Duplicated here to
// keep the migrate package free of any handler dependency; if the bucket name
// ever changes, both constants must move together.
const sgBucket = "spinifex-vpc-security-groups"

const sgMigrationMaxRetries = 3

// sgRule is the v1→v2 schema snapshot of handlers/ec2/vpc.SGRule. A frozen
// copy decouples this migration from future struct evolution.
type sgRule struct {
	RuleId     string `json:"rule_id"`
	IpProtocol string `json:"ip_protocol"`
	FromPort   int64  `json:"from_port"`
	ToPort     int64  `json:"to_port"`
	CidrIp     string `json:"cidr_ip,omitempty"`
	SourceSG   string `json:"source_sg,omitempty"`
}

// sgRecord is the v1→v2 schema snapshot of handlers/ec2/vpc.SecurityGroupRecord.
type sgRecord struct {
	GroupId      string            `json:"group_id"`
	GroupName    string            `json:"group_name"`
	Description  string            `json:"description"`
	VpcId        string            `json:"vpc_id"`
	IngressRules []sgRule          `json:"ingress_rules"`
	EgressRules  []sgRule          `json:"egress_rules"`
	Tags         map[string]string `json:"tags"`
	IsDefault    bool              `json:"is_default,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
}

func init() {
	DefaultRegistry.RegisterKV(sgBucket, KVMigration{
		FromVersion: 1,
		ToVersion:   2,
		Description: "assign sgr- IDs to security group rules",
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
				if err := backfillSGRuleIDs(ctx, key); err != nil {
					return err
				}
			}
			return nil
		},
	})
}

// backfillSGRuleIDs assigns sgr- IDs to any rule missing one. Skips records
// already fully populated. CAS conflicts retry up to sgMigrationMaxRetries.
func backfillSGRuleIDs(ctx KVContext, key string) error {
	var lastErr error
	for attempt := range sgMigrationMaxRetries {
		entry, err := ctx.KV.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				return nil
			}
			return fmt.Errorf("get %s: %w", key, err)
		}

		var record sgRecord
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
			// nats.ErrKeyExists = revision mismatch (JSStreamWrongLastSequence).
			if errors.Is(err, nats.ErrKeyExists) {
				ctx.Logger.Warn("SG migration CAS conflict, retrying", "key", key, "attempt", attempt+1)
				lastErr = err
				continue
			}
			return fmt.Errorf("update %s: %w", key, err)
		}
		return nil
	}
	return fmt.Errorf("backfill rule IDs for %s: exceeded %d CAS retries: %w", key, sgMigrationMaxRetries, lastErr)
}
