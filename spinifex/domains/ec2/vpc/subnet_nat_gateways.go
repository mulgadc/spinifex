package vpc

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// The NAT gateway service writes one key per gateway here, named by subnet, so
// DeleteSubnet reads only its own subnet's gateways. It lives in this package
// because natgw imports vpc.
const (
	KVBucketSubnetNatGateways        = "spinifex-vpc-subnet-nat-gateways"
	KVBucketSubnetNatGatewaysHistory = 1

	kvBucketNatGateways = "spinifex-vpc-nat-gateways"
)

// SubnetNatGatewayKey is the index key recording that natgwID is in subnetID.
func SubnetNatGatewayKey(accountID, subnetID, natgwID string) string {
	return kvstore.AccountKey(accountID, subnetID+"."+natgwID)
}

// checkSubnetNatGateways returns DependencyViolation while a NAT gateway is in
// the subnet. An index key counts only while its gateway record exists, since a
// deleted gateway leaves that bucket. Fail-closed on a KV read error.
func (s *VPCServiceImpl) checkSubnetNatGateways(ctx context.Context, accountID, subnetID string) error {
	prefix := SubnetNatGatewayKey(accountID, subnetID, "")
	lister, err := s.natgwIdxKV.ListKeysFiltered(ctx, prefix+"*")
	if err != nil {
		slog.ErrorContext(ctx, "DeleteSubnet: NAT gateway index read failed, blocking delete", "subnetId", subnetID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	var natgwIDs []string
	for key := range lister.Keys() {
		natgwIDs = append(natgwIDs, strings.TrimPrefix(key, prefix))
	}
	// Cancellation closes the lister early, and a short list must not read as empty.
	if err := ctx.Err(); err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	for _, natgwID := range natgwIDs {
		_, err := s.natgwKV.Get(ctx, kvstore.AccountKey(accountID, natgwID))
		if err == nil {
			return awserrors.HasDependencies("subnet", subnetID)
		}
		if !errors.Is(err, jetstream.ErrKeyNotFound) {
			slog.ErrorContext(ctx, "DeleteSubnet: NAT gateway read failed, blocking delete", "subnetId", subnetID, "natGatewayId", natgwID, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
	}
	return nil
}
