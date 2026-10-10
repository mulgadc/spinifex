package awsgw

import (
	"testing"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeAccountLister struct {
	accounts []*handlers_iam.Account
	err      error
}

func (f *fakeAccountLister) ListAccounts() ([]*handlers_iam.Account, error) {
	return f.accounts, f.err
}

func TestActiveAccountIDs_FiltersActive(t *testing.T) {
	lister := &fakeAccountLister{accounts: []*handlers_iam.Account{
		{AccountID: "000000000001", Status: handlers_iam.AccountStatusActive},
		{AccountID: "000000000002", Status: "SUSPENDED"},
		{AccountID: "000000000003", Status: handlers_iam.AccountStatusActive},
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
