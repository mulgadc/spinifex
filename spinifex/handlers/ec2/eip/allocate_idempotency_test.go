package handlers_ec2_eip

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The SDK reuses amz-sdk-invocation-id across retries of one call, so a resend
// must return the first allocation instead of drawing another address.
func TestAllocateAddress_RetryReturnsFirstAllocation(t *testing.T) {
	svc, _ := setupTestEIP(t)
	ctx := utils.WithIdempotencyKey(t.Context(), "invocation-1")

	first, err := svc.AllocateAddress(ctx, &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	second, err := svc.AllocateAddress(ctx, &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, *first.AllocationId, *second.AllocationId)
	assert.Equal(t, *first.PublicIp, *second.PublicIp)

	got, err := svc.DescribeAddresses(t.Context(), &ec2.DescribeAddressesInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, got.Addresses, 1, "retry must not leave a second address held with no owner")
}

// Distinct calls carry distinct invocation ids and must still get their own EIP.
func TestAllocateAddress_DifferentKeysAllocateSeparately(t *testing.T) {
	svc, _ := setupTestEIP(t)

	first, err := svc.AllocateAddress(utils.WithIdempotencyKey(t.Context(), "invocation-1"), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	second, err := svc.AllocateAddress(utils.WithIdempotencyKey(t.Context(), "invocation-2"), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	assert.NotEqual(t, *first.PublicIp, *second.PublicIp)
}

// Callers that send no token (an older SDK, a raw HTTP client) keep the
// pre-existing behaviour rather than colliding on an empty key.
func TestAllocateAddress_WithoutKeyAllocatesEachTime(t *testing.T) {
	svc, _ := setupTestEIP(t)

	first, err := svc.AllocateAddress(t.Context(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	second, err := svc.AllocateAddress(t.Context(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)

	assert.NotEqual(t, *first.PublicIp, *second.PublicIp)
}

// One account's token must not answer another's call.
func TestAllocateAddress_KeyIsScopedPerAccount(t *testing.T) {
	svc, _ := setupTestEIP(t)
	ctx := utils.WithIdempotencyKey(t.Context(), "invocation-1")

	first, err := svc.AllocateAddress(ctx, &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	second, err := svc.AllocateAddress(ctx, &ec2.AllocateAddressInput{}, "210987654321")
	require.NoError(t, err)

	assert.NotEqual(t, *first.PublicIp, *second.PublicIp)
}

// The retry usually arrives while the first DORA is still running, which the KV
// record cannot cover — singleflight has to join it to the call in flight.
func TestAllocateOnce_ConcurrentRetriesCollapse(t *testing.T) {
	svc, _ := setupTestEIP(t)
	ctx := utils.WithIdempotencyKey(t.Context(), "invocation-1")

	var calls atomic.Int32
	release := make(chan struct{})
	alloc := func() (*ec2.AllocateAddressOutput, error) {
		calls.Add(1)
		<-release
		return &ec2.AllocateAddressOutput{
			AllocationId: aws.String("eipalloc-abc"),
			PublicIp:     aws.String("198.51.100.11"),
		}, nil
	}

	const attempts = 4
	outs := make([]*ec2.AllocateAddressOutput, attempts)
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Go(func() {
			outs[i], errs[i] = svc.allocateOnce(ctx, testAccountID, alloc)
		})
	}
	// The first alloc is pinned until every attempt has had time to join it, so
	// all four are genuinely in flight together rather than run back to back.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	for i := range attempts {
		require.NoError(t, errs[i])
		assert.Equal(t, "eipalloc-abc", *outs[i].AllocationId)
	}
	assert.Equal(t, int32(1), calls.Load(), "concurrent retries must share one allocation")
}

// A failed allocation must not be cached: the caller is entitled to retry and
// get a real address once the fault clears.
func TestAllocateOnce_FailureIsNotCached(t *testing.T) {
	svc, _ := setupTestEIP(t)
	ctx := utils.WithIdempotencyKey(t.Context(), "invocation-1")

	_, err := svc.allocateOnce(ctx, testAccountID, func() (*ec2.AllocateAddressOutput, error) {
		return nil, errors.New("pool unreachable")
	})
	require.Error(t, err)

	out, err := svc.allocateOnce(ctx, testAccountID, func() (*ec2.AllocateAddressOutput, error) {
		return &ec2.AllocateAddressOutput{
			AllocationId: aws.String("eipalloc-abc"),
			PublicIp:     aws.String("198.51.100.11"),
		}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, "eipalloc-abc", *out.AllocationId)
}

// Without a bucket the dedupe degrades to the in-flight join only, which must
// not turn into a nil dereference.
func TestAllocateOnce_WithoutBucketStillAllocates(t *testing.T) {
	svc, _ := setupTestEIP(t)
	svc.idemKV = nil

	out, err := svc.allocateOnce(utils.WithIdempotencyKey(t.Context(), "invocation-1"), testAccountID,
		func() (*ec2.AllocateAddressOutput, error) {
			return &ec2.AllocateAddressOutput{AllocationId: aws.String("eipalloc-abc")}, nil
		})
	require.NoError(t, err)
	assert.Equal(t, "eipalloc-abc", *out.AllocationId)
}

func TestIdempotencyKeyHash_IsStableAndKVSafe(t *testing.T) {
	got := idempotencyKeyHash("some/token with spaces.and*wildcards")

	assert.Equal(t, got, idempotencyKeyHash("some/token with spaces.and*wildcards"))
	assert.NotEqual(t, got, idempotencyKeyHash("another token"))
	assert.Regexp(t, `^[0-9a-f]{32}$`, got)
}
