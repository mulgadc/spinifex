package accountadmission

import (
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The gateway's Admitter and a separate writer (standing in for the CLI process) share
// one JetStream. A status written by the writer must be seen by the very next request,
// with no wait, because the Admitter keeps no copy of an earlier answer.
func TestAdmit_SeesAStatusWrittenByAnotherClientOnTheNextRequest(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)
	gatewayIAM, err := iamdomain.NewIAMServiceImpl(t.Context(), nc, masterKey)
	require.NoError(t, err)
	writer, err := iamdomain.NewIAMServiceImpl(t.Context(), nc, masterKey)
	require.NoError(t, err)

	// A fresh service hands out 000000000001 first, an ID IAM never moves off ACTIVE.
	_, err = writer.CreateAccount("reserved-admin")
	require.NoError(t, err)
	account, err := writer.CreateAccount("warm")
	require.NoError(t, err)
	bystander, err := writer.CreateAccount("bystander")
	require.NoError(t, err)
	admitter := New(gatewayIAM)

	// Warm both accounts: an implementation that cached ACTIVE would hold this answer.
	require.NoError(t, admitter.Admit(account.AccountID, ""))
	require.NoError(t, admitter.Admit(bystander.AccountID, ""))

	_, err = writer.SetAccountStatus(account.AccountID, iamdomain.AccountStatusTerminating)
	require.NoError(t, err)

	assert.ErrorIs(t, admitter.Admit(account.AccountID, ""), ErrNotActive, "the next request must see TERMINATING")
	assert.NoError(t, admitter.Admit(bystander.AccountID, ""), "another account is unaffected")
}
