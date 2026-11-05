package instance

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The password is handed straight to the agent and never persisted, so the
// command is the only place it exists between the request and the engine.
func TestSetMasterPassword_CarriesTheCredentialsToTheAgent(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)

	require.NoError(t, h.svc.setMasterPassword(t.Context(), testAccountID, testDBID, "postgres", "n3w-pw"))

	issued := h.agent.received()
	require.Len(t, issued, 1)
	assert.Equal(t, CommandSetPassword, issued[0].Type)
	assert.NotEmpty(t, issued[0].CommandID)
	assert.ElementsMatch(t, []Parameter{
		{Name: CommandParamMasterUsername, Value: "postgres"},
		{Name: CommandParamMasterUserPassword, Value: "n3w-pw"},
	}, issued[0].Parameters)
}

// A rotation that could not be applied has to fail loudly: reporting success
// would leave the customer with a password the engine does not accept.
func TestSetMasterPassword_FailsWhenTheAgentRejectsIt(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, true)

	err := h.svc.setMasterPassword(t.Context(), testAccountID, testDBID, "postgres", "n3w-pw")
	require.Error(t, err)
	assert.Contains(t, err.Error(), CommandSetPassword)
}

// The settings the engine accepted but will not honour until it restarts
// come back as the reply message, which is what a later reboot then clears.
func TestApplyParameters_ReportsWhatIsPendingARestart(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	h.agent.replyWith("shared_buffers, max_connections")

	pending, err := h.svc.applyParameters(t.Context(), testAccountID, testDBID,
		[]Parameter{{Name: "shared_buffers", Value: "256MB"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"shared_buffers", "max_connections"}, pending)

	issued := h.agent.received()
	require.Len(t, issued, 1)
	assert.Equal(t, CommandApplyParams, issued[0].Type)
	assert.Equal(t, []Parameter{{Name: "shared_buffers", Value: "256MB"}}, issued[0].Parameters)
}

// An agent that reports nothing pending has applied everything live.
func TestApplyParameters_ReportsNothingPendingOnAnEmptyReply(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)

	pending, err := h.svc.applyParameters(t.Context(), testAccountID, testDBID,
		[]Parameter{{Name: "work_mem", Value: "8MB"}})
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// The reply is matched on the command ID, so a stale answer to an earlier
// issuer cannot be mistaken for the outcome of this command.
func TestIssueCommand_CorrelatesTheReplyByCommandID(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)

	reply, err := h.svc.issueCommand(t.Context(), testAccountID, testDBID,
		CommandStopEngine, stopEngineTimeout, nil)
	require.NoError(t, err)
	require.NotNil(t, reply)

	issued := h.agent.received()
	require.NotEmpty(t, issued)
	assert.Equal(t, issued[0].CommandID, reply.CommandID)
}

// Each copy carries what its issuer has left, so a copy that reaches the agent
// late cannot be granted a whole budget its issuer is about to give up on.
func TestIssueCommand_StampsEachCopyWithTheBudgetLeft(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	h.agent.silenceType(CommandQuiesce)

	// A republish can only fire late, never early, so consecutive stamps differ
	// by at least the interval however loaded the runner is. The budget leaves
	// the second copy most of a second of slack.
	const republishEvery = 100 * time.Millisecond
	h.svc.deps.CommandRepublishEvery = republishEvery

	const budget = time.Second
	_, err := h.svc.issueCommand(t.Context(), testAccountID, testDBID, CommandQuiesce, budget, nil)
	require.ErrorIs(t, err, ErrCommandUnreachable)

	issued := h.agent.received()
	require.GreaterOrEqual(t, len(issued), 2, "the issuer must have re-published at least once")
	for i, copied := range issued {
		assert.Positive(t, copied.RemainingBudgetMs, "copy %d carries no remaining budget", i)
		assert.LessOrEqual(t, copied.RemainingBudgetMs, budget.Milliseconds(), "copy %d", i)
		if i > 0 {
			assert.LessOrEqual(t, copied.RemainingBudgetMs, issued[i-1].RemainingBudgetMs-republishEvery.Milliseconds()/2,
				"copy %d was stamped with as much budget as the copy before it", i)
		}
	}
}
