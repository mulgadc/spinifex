package accountteardown

import (
	"context"
	"errors"
	"fmt"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/nats-io/nats.go/jetstream"
)

// AccountReapers returns the final-stage reapers: the per-account counters and
// reservations that outlive every resource but must not outlive the account.
func AccountReapers(svc handlers_iam.IAMService, names *handlers_iam.AccountNameIndex, usage jetstream.KeyValue) []Reaper {
	return []Reaper{
		&quotaUsageReaper{usage: usage},
		&nameReservationReaper{svc: svc, names: names},
	}
}

// quotaUsageReaper removes the account's vCPU counter. Left behind it is a
// small leak that also makes the account id look live to anything reading the
// bucket to decide what exists.
type quotaUsageReaper struct{ usage jetstream.KeyValue }

func (r *quotaUsageReaper) Kind() string { return "quota-usage" }
func (r *quotaUsageReaper) Stage() Stage { return StageAccount }

func (r *quotaUsageReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	if r.usage == nil {
		return nil, nil
	}
	if _, err := r.usage.Get(ctx, accountID); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("read quota usage: %w", err)
	}
	return []Resource{{Kind: r.Kind(), ID: accountID}}, nil
}

func (r *quotaUsageReaper) Delete(ctx context.Context, _ string, resource Resource, _ bool) error {
	if r.usage == nil {
		return nil
	}
	if err := r.usage.Delete(ctx, resource.ID); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return err
	}
	return nil
}

// nameReservationReaper releases the account's name so the same email can sign
// up again. A customer who deletes an account and returns must not be told the
// address is taken by a tenant that no longer exists.
type nameReservationReaper struct {
	svc   handlers_iam.IAMService
	names *handlers_iam.AccountNameIndex
}

func (r *nameReservationReaper) Kind() string { return "name-reservation" }
func (r *nameReservationReaper) Stage() Stage { return StageAccount }

func (r *nameReservationReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	if r.names == nil {
		return nil, nil
	}
	account, err := r.svc.GetAccount(accountID)
	if err != nil {
		// The account record is deleted after this stage, so it must still be
		// here. Its absence means someone else removed it and the reservation
		// can no longer be attributed — report rather than guess.
		return nil, fmt.Errorf("read account for its name reservation: %w", err)
	}

	reserved, found, err := r.names.Lookup(ctx, account.AccountName)
	if err != nil {
		return nil, fmt.Errorf("look up name reservation: %w", err)
	}
	if !found || reserved != accountID {
		return nil, nil
	}
	return []Resource{{Kind: r.Kind(), ID: account.AccountName}}, nil
}

func (r *nameReservationReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	if r.names == nil {
		return nil
	}
	return r.names.ReleaseDeleted(ctx, resource.ID, accountID)
}

// IAMAccounts adapts the IAM service to the engine's AccountStore. The engine
// declares its own Account type so its tests need no cluster; this is the one
// place the two meet.
type IAMAccounts struct{ svc handlers_iam.IAMService }

var _ AccountStore = (*IAMAccounts)(nil)

func NewIAMAccounts(svc handlers_iam.IAMService) *IAMAccounts {
	return &IAMAccounts{svc: svc}
}

func (a *IAMAccounts) GetAccount(accountID string) (*Account, error) {
	account, err := a.svc.GetAccount(accountID)
	if err != nil {
		return nil, err
	}
	return &Account{
		AccountID: account.AccountID, AccountName: account.AccountName, Status: account.Status,
	}, nil
}

func (a *IAMAccounts) SetAccountStatus(accountID, status string) (*Account, error) {
	account, err := a.svc.SetAccountStatus(accountID, status)
	if err != nil {
		return nil, err
	}
	return &Account{
		AccountID: account.AccountID, AccountName: account.AccountName, Status: account.Status,
	}, nil
}

func (a *IAMAccounts) DeleteAccount(accountID string) error {
	return a.svc.DeleteAccount(accountID)
}
