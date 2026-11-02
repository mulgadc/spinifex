// Package accountadmission decides whether a request's account may proceed. The SigV4
// gateway path and the ECR registry bridge both ask it, so account status has one
// enforcement point.
package accountadmission

import (
	"errors"
	"fmt"
	"log/slog"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
)

// ErrNotActive means the account exists but is suspended or being torn down.
var ErrNotActive = credential.ErrAccountNotActive

// Admitter reads account status through the in-process IAM service on every request.
// It keeps no copy of an earlier answer, so a status written by any process is seen by
// the next request.
type Admitter struct {
	iam iamdomain.IAMService
}

// New returns an Admitter. A nil iam fails every admission closed.
func New(iam iamdomain.IAMService) *Admitter {
	return &Admitter{iam: iam}
}

// Admit returns nil when the account may proceed, ErrNotActive when it may not, and
// a missing record too, and any other error when its status could not be read. An unreadable account is not
// evidence of one in good standing, so callers must fail closed on every error. An
// empty accountID is admitted: the caller has no account to gate. logIP is for log
// lines only.
func (a *Admitter) Admit(accountID, logIP string) error {
	if accountID == "" {
		return nil
	}
	if a == nil || a.iam == nil {
		slog.Error("Auth: IAM service not initialized for account status check")
		return errors.New("IAM service not initialized")
	}

	account, err := a.iam.GetAccount(accountID)
	if awserrors.IsErrorCode(err, awserrors.ErrorIAMNoSuchEntity) {
		// The in-process read is the complete view, so an absent record is a denial
		// rather than a dependency failure.
		slog.Warn("Auth failure: account does not exist", "accountID", accountID, "sourceIP", logIP)
		return ErrNotActive
	}
	if err != nil {
		// This gate is what stops a terminating tenant from creating resources the
		// teardown has already walked past.
		slog.Error("Auth: failed to read account for status check", "accountID", accountID, "err", err)
		return fmt.Errorf("read account: %w", err)
	}

	if account.Status != iamdomain.AccountStatusActive {
		slog.Warn("Auth failure: account is not active",
			"accountID", accountID, "status", account.Status, "sourceIP", logIP)
		return ErrNotActive
	}
	return nil
}

// Active reports whether the account may proceed. A suspended, terminating or deleted
// account is (false, nil); only an unreadable status is an error.
func (a *Admitter) Active(accountID, logIP string) (bool, error) {
	err := a.Admit(accountID, logIP)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotActive), awserrors.IsErrorCode(err, awserrors.ErrorIAMNoSuchEntity):
		return false, nil
	default:
		return false, err
	}
}
