package accountadmission

import (
	"errors"
	"testing"
	"time"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeIAM struct {
	iamdomain.IAMService

	status string
	err    error
	reads  int
}

func (f *fakeIAM) GetAccount(accountID string) (*iamdomain.Account, error) {
	f.reads++
	if f.err != nil {
		return nil, f.err
	}
	return &iamdomain.Account{AccountID: accountID, Status: f.status}, nil
}

func TestAdmit(t *testing.T) {
	t.Run("active account", func(t *testing.T) {
		require.NoError(t, New(&fakeIAM{status: iamdomain.AccountStatusActive}).Admit("1", ""))
	})
	for _, status := range []string{iamdomain.AccountStatusSuspended, iamdomain.AccountStatusTerminating} {
		t.Run(status, func(t *testing.T) {
			assert.ErrorIs(t, New(&fakeIAM{status: status}).Admit("1", ""), ErrNotActive)
		})
	}
	t.Run("unreadable account fails closed", func(t *testing.T) {
		err := New(&fakeIAM{err: errors.New("store down")}).Admit("1", "")
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrNotActive)
	})
	t.Run("nil IAM fails closed", func(t *testing.T) {
		err := New(nil).Admit("1", "")
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrNotActive)
	})
	t.Run("no account is admitted", func(t *testing.T) {
		require.NoError(t, New(nil).Admit("", ""))
	})
}

func TestAdmit_CachesOnlyActive(t *testing.T) {
	svc := &fakeIAM{status: iamdomain.AccountStatusActive}
	a := New(svc)
	a.watching.Store(true)
	require.NoError(t, a.Admit("1", ""))
	require.NoError(t, a.Admit("1", ""))
	assert.Equal(t, 1, svc.reads, "a warm ACTIVE entry is not re-read")

	svc.status = iamdomain.AccountStatusSuspended
	a.evictForTest("1")
	assert.ErrorIs(t, a.Admit("1", ""), ErrNotActive)
	assert.ErrorIs(t, a.Admit("1", ""), ErrNotActive)
	assert.Equal(t, 3, svc.reads, "a non-active answer is never cached")
}

func TestAdmit_WithoutAWatcherEveryRequestReads(t *testing.T) {
	svc := &fakeIAM{status: iamdomain.AccountStatusActive}
	a := New(svc)
	require.NoError(t, a.Admit("1", ""))
	require.NoError(t, a.Admit("1", ""))
	assert.Equal(t, 2, svc.reads, "an unwatched cache could hold a stale ACTIVE for the whole TTL")
}

func TestActiveEntryExpires(t *testing.T) {
	a := New(&fakeIAM{status: iamdomain.AccountStatusActive})
	now := time.Now()
	assert.False(t, a.activeUntil("1", now), "an unseen account must not read as active")
	a.markActive("1", 0, now)
	assert.True(t, a.activeUntil("1", now))
	assert.False(t, a.activeUntil("1", now.Add(ActiveTTL+time.Second)), "the entry must expire with the TTL")
}

func TestMarkActiveSkipsARecordTheWatcherHasAlreadySupersededAfterTheRead(t *testing.T) {
	a := New(&fakeIAM{status: iamdomain.AccountStatusActive})
	a.latest["1"] = 2
	a.markActive("1", 1, time.Now())
	assert.False(t, a.activeUntil("1", time.Now()), "a read older than the newest watched record must not be cached")
	a.markActive("1", 2, time.Now())
	assert.True(t, a.activeUntil("1", time.Now()))
}

func TestNilAdmitterReadsNothing(t *testing.T) {
	var a *Admitter
	assert.False(t, a.activeUntil("1", time.Now()))
}

func (a *Admitter) evictForTest(accountID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.active, accountID)
}

func TestActive(t *testing.T) {
	cases := []struct {
		name    string
		iam     iamdomain.IAMService
		active  bool
		wantErr bool
	}{
		{"active", &fakeIAM{status: iamdomain.AccountStatusActive}, true, false},
		{"suspended", &fakeIAM{status: iamdomain.AccountStatusSuspended}, false, false},
		{"terminating", &fakeIAM{status: iamdomain.AccountStatusTerminating}, false, false},
		{"deleted", &fakeIAM{err: errors.New(awserrors.ErrorIAMNoSuchEntity)}, false, false},
		{"unreadable", &fakeIAM{err: errors.New("store down")}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			active, err := New(c.iam).Active("1", "")
			assert.Equal(t, c.active, active)
			assert.Equal(t, c.wantErr, err != nil)
		})
	}
}
