package awsgw

import (
	"testing"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeAccountLister struct {
	accounts []*iamdomain.Account
	err      error
}

func (f *fakeAccountLister) ListAccounts() ([]*iamdomain.Account, error) {
	return f.accounts, f.err
}

func TestActiveAccountIDs_FiltersActive(t *testing.T) {
	lister := &fakeAccountLister{accounts: []*iamdomain.Account{
		{AccountID: "000000000001", Status: iamdomain.AccountStatusActive},
		{AccountID: "000000000002", Status: "SUSPENDED"},
		{AccountID: "000000000003", Status: iamdomain.AccountStatusActive},
	}}

	ids, err := activeAccountIDs(lister)()
	require.NoError(t, err)
	assert.Equal(t, []string{"000000000001", "000000000003"}, ids)
}

func TestActiveAccountIDs_Empty(t *testing.T) {
	ids, err := activeAccountIDs(&fakeAccountLister{})()
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestActiveAccountIDs_Error(t *testing.T) {
	ids, err := activeAccountIDs(&fakeAccountLister{err: assert.AnError})()
	require.Error(t, err)
	assert.Nil(t, ids)
}
