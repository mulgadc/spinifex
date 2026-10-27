package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	ec2account "github.com/mulgadc/spinifex/spinifex/domains/ec2/account"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sealTestChecker answers AccountTerminating from a fixed set, standing in
// for IAM in these daemon-level tests.
type sealTestChecker struct {
	terminating map[string]bool
}

func (c *sealTestChecker) AccountTerminating(_ context.Context, accountID string) (bool, error) {
	return c.terminating[accountID], nil
}

// newSealTestDaemon builds a daemon serving its real subscription table over
// a real account settings service, wired with checker as its termination
// precondition.
func newSealTestDaemon(t *testing.T, checker ec2account.AccountTerminationChecker) (*Daemon, *nats.Conn) {
	t.Helper()
	ns, nc, _ := testutil.StartTestJetStream(t)

	acctSvc, err := ec2account.NewAccountSettingsServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	acctSvc.SetAccountTerminationChecker(checker)

	d := newSubscribeTestDaemon(t, nc, "", "")
	d.accountService = acctSvc
	require.NoError(t, d.subscribeAll())
	require.NoError(t, nc.Flush())

	caller, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(caller.Close)
	return d, caller
}

const (
	sealTestTerminating = "111122223333"
	sealTestActive      = "444455556666"
)

func sealRequest(t *testing.T, nc *nats.Conn, body any, header string) []byte {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	reply, err := natsRequestAs(nc, ec2v1.SealAccountSettingsSubject, data, header, 5*time.Second)
	require.NoError(t, err)
	return reply.Data
}

// The header is ignored: the body account is what gets sealed, even when
// X-Account-ID names a different one.
func TestSealAccountSettingsSubjectSealsTheBodyAccount(t *testing.T) {
	_, nc := newSealTestDaemon(t, &sealTestChecker{terminating: map[string]bool{sealTestTerminating: true}})

	raw := sealRequest(t, nc, ec2v1.SealAccountSettingsRequest{AccountID: sealTestTerminating, CorrelationRef: "req-1"}, sealTestActive)
	require.Empty(t, replyErrCode(t, raw), string(raw))

	var reply ec2v1.SealAccountSettingsReply
	require.NoError(t, json.Unmarshal(raw, &reply))
	assert.Equal(t, sealTestTerminating, reply.AccountID)
	assert.Equal(t, "req-1", reply.CorrelationRef)
	assert.Equal(t, ec2v1.AccountSettingsDispositionSealed, reply.Disposition)
	assert.NotZero(t, reply.Revision)
}

// Only an account IAM holds as TERMINATING may be sealed; ACTIVE is refused
// before any write.
func TestSealAccountSettingsSubjectRefusesAnActiveAccount(t *testing.T) {
	_, nc := newSealTestDaemon(t, &sealTestChecker{terminating: map[string]bool{sealTestTerminating: true}})

	raw := sealRequest(t, nc, ec2v1.SealAccountSettingsRequest{AccountID: sealTestActive}, "")
	assert.Equal(t, awserrors.ErrorIncorrectState, replyErrCode(t, raw))
}

// A request without a valid account is refused, not sealed.
func TestSealAccountSettingsSubjectRefusesAMissingAccount(t *testing.T) {
	_, nc := newSealTestDaemon(t, &sealTestChecker{terminating: map[string]bool{sealTestTerminating: true}})

	assert.Equal(t, awserrors.ErrorMissingParameter, replyErrCode(t, sealRequest(t, nc, ec2v1.SealAccountSettingsRequest{}, "")))
}

// A daemon that does not serve the subject leaves the request with no responder.
func TestSealAccountSettingsSubjectHasNoResponderWithoutTheDaemon(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	_, err := natsRequestAs(nc, ec2v1.SealAccountSettingsSubject, []byte(`{"account_id":"111122223333"}`), "", time.Second)
	require.ErrorIs(t, err, nats.ErrNoResponders)
}
