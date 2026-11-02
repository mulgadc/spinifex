package accountadmission

import (
	"context"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// watchedPair is two IAM services over one JetStream, standing in for the gateway and
// a separate CLI process that writes account status.
type watchedPair struct {
	gateway *Admitter
	cli     iamdomain.IAMService
	account string
	bucket  jetstream.KeyValue
}

func newWatchedPair(t *testing.T) (*watchedPair, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)

	gatewayIAM, err := iamdomain.NewIAMServiceImpl(t.Context(), nc, masterKey)
	require.NoError(t, err)
	cli, err := iamdomain.NewIAMServiceImpl(t.Context(), nc, masterKey)
	require.NoError(t, err)
	account, err := cli.CreateAccount("watched")
	require.NoError(t, err)

	bucket, err := js.KeyValue(t.Context(), iamdomain.KVBucketAccounts)
	require.NoError(t, err)

	admitter := New(gatewayIAM)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done, err := admitter.Watch(ctx, bucket)
	require.NoError(t, err)
	return &watchedPair{gateway: admitter, cli: cli, account: account.AccountID, bucket: bucket}, cancel, done
}

func TestWatchEvictsOnAStatusChangeFromAnotherClient(t *testing.T) {
	p, _, _ := newWatchedPair(t)
	require.NoError(t, p.gateway.Admit(p.account, ""))
	require.True(t, p.gateway.activeUntil(p.account, time.Now()), "the ACTIVE answer must be cached before the change")

	_, err := p.cli.SetAccountStatus(p.account, iamdomain.AccountStatusTerminating)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return p.gateway.Admit(p.account, "") != nil
	}, ActiveTTL-time.Second, 10*time.Millisecond, "eviction must beat the TTL")
}

func TestWatchEvictsOnADelete(t *testing.T) {
	p, _, _ := newWatchedPair(t)
	_, err := p.cli.SetAccountStatus(p.account, iamdomain.AccountStatusTerminating)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return p.gateway.Admit(p.account, "") != nil }, 2*time.Second, 10*time.Millisecond)

	other, err := p.cli.CreateAccount("to-delete")
	require.NoError(t, err)
	require.NoError(t, p.gateway.Admit(other.AccountID, ""))
	require.True(t, p.gateway.activeUntil(other.AccountID, time.Now()))

	require.NoError(t, p.bucket.Delete(t.Context(), other.AccountID))
	require.Eventually(t, func() bool {
		return !p.gateway.activeUntil(other.AccountID, time.Now())
	}, ActiveTTL-time.Second, 10*time.Millisecond, "a deleted account must leave the cache before the TTL")
}

func TestWatchKeepsOtherAccountsCached(t *testing.T) {
	p, _, _ := newWatchedPair(t)
	other, err := p.cli.CreateAccount("bystander")
	require.NoError(t, err)
	require.NoError(t, p.gateway.Admit(other.AccountID, ""))

	_, err = p.cli.SetAccountStatus(p.account, iamdomain.AccountStatusTerminating)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return p.gateway.Admit(p.account, "") != nil }, 2*time.Second, 10*time.Millisecond)

	assert.True(t, p.gateway.activeUntil(other.AccountID, time.Now()), "another account's entry must survive")
}

func TestStoppedWatcherBypassesTheCache(t *testing.T) {
	p, cancel, done := newWatchedPair(t)
	require.NoError(t, p.gateway.Admit(p.account, ""))

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher goroutine did not stop with its context")
	}

	_, err := p.cli.SetAccountStatus(p.account, iamdomain.AccountStatusTerminating)
	require.NoError(t, err)
	assert.ErrorIs(t, p.gateway.Admit(p.account, ""), ErrNotActive, "with no watcher the change must be seen at once")
}
