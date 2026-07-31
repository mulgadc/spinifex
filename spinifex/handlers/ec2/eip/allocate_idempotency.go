package handlers_ec2_eip

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// KVBucketEIPIdempotency records the outcome of an AllocateAddress keyed by
	// the caller's retry token, so a resend returns the first result instead of
	// allocating again.
	KVBucketEIPIdempotency = "spinifex-vpc-eip-idempotency"
	// eipIdempotencyTTL outlives the SDK retry window without pinning keys
	// forever. Entries age out on their own; nothing sweeps this bucket.
	eipIdempotencyTTL = 30 * time.Minute
)

// allocateOnce collapses retries of a single AllocateAddress call. vpcd's DORA
// ladder can outlast the AWS client read timeout, and EC2 AllocateAddress takes
// no client token, so without this each SDK retry minted a fresh allocation ID
// and took another upstream DHCP lease — returning one EIP to the caller and
// stranding the rest with nothing to release them.
func (s *EIPServiceImpl) allocateOnce(ctx context.Context, accountID string, alloc func() (*ec2.AllocateAddressOutput, error)) (*ec2.AllocateAddressOutput, error) {
	key := utils.IdempotencyKeyFromContext(ctx)
	if key == "" || s.idemKV == nil {
		return alloc()
	}
	kvKey := utils.AccountKey(accountID, idempotencyKeyHash(key))

	if out := s.cachedAllocation(ctx, kvKey); out != nil {
		slog.WarnContext(ctx, "AllocateAddress: returning first result for a retried request",
			"allocationId", *out.AllocationId, "accountID", accountID)
		return out, nil
	}

	// Retries usually arrive while the first call is still in DORA, so the
	// in-flight join is the common path and the KV record covers the rest.
	res, err, _ := s.allocSF.Do(kvKey, func() (any, error) {
		if out := s.cachedAllocation(ctx, kvKey); out != nil {
			return out, nil
		}
		out, err := alloc()
		if err != nil {
			return nil, err
		}
		s.recordAllocation(ctx, kvKey, out)
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	out, ok := res.(*ec2.AllocateAddressOutput)
	if !ok {
		return nil, fmt.Errorf("allocate address: unexpected singleflight result %T", res)
	}
	return out, nil
}

// idempotencyKeyHash renders a caller-supplied token as a KV-safe key. NATS
// restricts the key alphabet, and the token comes off an HTTP header, so it is
// hashed rather than trusted verbatim.
func idempotencyKeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

// cachedAllocation returns a previously recorded outcome, or nil when there is
// none. A corrupt or unreadable record is treated as absent: re-allocating
// costs an address, but failing the call outright would deny a legitimate one.
func (s *EIPServiceImpl) cachedAllocation(ctx context.Context, kvKey string) *ec2.AllocateAddressOutput {
	entry, err := s.idemKV.Get(ctx, kvKey)
	if err != nil {
		if !errors.Is(err, jetstream.ErrKeyNotFound) {
			slog.WarnContext(ctx, "AllocateAddress: idempotency lookup failed; treating as a new request", "err", err)
		}
		return nil
	}
	var out ec2.AllocateAddressOutput
	if err := json.Unmarshal(entry.Value(), &out); err != nil || out.AllocationId == nil {
		slog.WarnContext(ctx, "AllocateAddress: unreadable idempotency record; treating as a new request", "err", err)
		return nil
	}
	return &out
}

// recordAllocation stores the outcome so a retry arriving after the first call
// completed still gets the same allocation. A write failure only costs the
// dedupe, so the allocation itself is not unwound.
func (s *EIPServiceImpl) recordAllocation(ctx context.Context, kvKey string, out *ec2.AllocateAddressOutput) {
	data, err := json.Marshal(out)
	if err != nil {
		slog.WarnContext(ctx, "AllocateAddress: marshal idempotency record failed", "err", err)
		return
	}
	if _, err := s.idemKV.Put(ctx, kvKey, data); err != nil {
		slog.WarnContext(ctx, "AllocateAddress: recording idempotency key failed; a retry may allocate again", "err", err)
	}
}
