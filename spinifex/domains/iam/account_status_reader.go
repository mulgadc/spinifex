package iam

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// AccountStatusReader reads account status from spinifex-accounts and never
// writes. It is the read-only capability EC2's settings seal and the quota
// override seal both use to check their TERMINATING precondition.
type AccountStatusReader struct {
	accounts *kvstore.Bucket // AttachOnly: never creates the bucket
}

// NewAccountStatusReader attaches to the existing spinifex-accounts bucket.
// It needs no master key and runs no migration: a caller that only reads
// account status must not bootstrap IAM's bucket out from under it.
func NewAccountStatusReader(js jetstream.JetStream) *AccountStatusReader {
	return &AccountStatusReader{
		accounts: kvstore.NewBucket(js, kvstore.Config{
			Name:       KVBucketAccounts,
			Missing:    "account status reader: nil jetstream client",
			AttachOnly: true,
		}),
	}
}

// AccountTerminating is a direct read, which suffices because TERMINATING is
// one-way until the record is deleted. A missing record is (false, nil); only
// a failed read or decode is an error.
func (r *AccountStatusReader) AccountTerminating(ctx context.Context, accountID string) (bool, error) {
	kv, err := r.accounts.KV(ctx)
	if err != nil {
		return false, err
	}

	entry, err := kv.Get(ctx, accountID)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return false, nil
		}
		return false, err
	}

	var record struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return false, err
	}
	return record.Status == AccountStatusTerminating, nil
}
