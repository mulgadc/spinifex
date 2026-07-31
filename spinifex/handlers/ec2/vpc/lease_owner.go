package handlers_ec2_vpc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

// ENIExists reports whether an ENI record survives, without knowing its
// account. Used to decide whether a DHCP lease still has an owner, so a listing
// failure is an error rather than a false — releasing an address on a KV
// timeout would be worse than keeping an orphan.
func (s *VPCServiceImpl) ENIExists(ctx context.Context, eniID string) (bool, error) {
	if eniID == "" {
		return false, errors.New("ENI exists: eniID required")
	}
	return keyWithSuffixExists(ctx, s.eniKV, eniID)
}

// VPCExists reports whether a VPC record survives, without knowing its account.
// Same contract as ENIExists.
func (s *VPCServiceImpl) VPCExists(ctx context.Context, vpcID string) (bool, error) {
	if vpcID == "" {
		return false, errors.New("VPC exists: vpcID required")
	}
	return keyWithSuffixExists(ctx, s.vpcKV, vpcID)
}

// keyWithSuffixExists looks for an account-scoped key ending in the resource id.
// Keys alone are enough — the record's contents do not change the verdict.
func keyWithSuffixExists(ctx context.Context, kv jetstream.KeyValue, resourceID string) (bool, error) {
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return false, nil
		}
		return false, fmt.Errorf("list records for %s: %w", resourceID, err)
	}
	suffix := "." + resourceID
	for _, k := range keys {
		if k != utils.VersionKey && strings.HasSuffix(k, suffix) {
			return true, nil
		}
	}
	return false, nil
}
