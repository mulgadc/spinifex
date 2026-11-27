// Package accounttermination is the migration domain's account-termination
// participant: its owned inventory and its termination command.
package accounttermination

import (
	"context"
	"errors"
	"log/slog"
	"time"

	accountlifecyclev1 "github.com/mulgadc/spinifex/contracts/accountlifecycle/v1"
	migrationv1 "github.com/mulgadc/spinifex/contracts/migration/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/migration/admission"
	"github.com/mulgadc/spinifex/spinifex/domains/migration/executor"
	"github.com/mulgadc/spinifex/spinifex/domains/migration/store"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/nats-io/nats.go/jetstream"
)

// Participant serves the inventory and the termination command.
type Participant struct {
	js          jetstream.KeyValueManager
	terminating admission.TerminationChecker
	seals       *admission.Seals
	exec        *executor.Executor
	cache       executor.SessionCache
	now         func() time.Time
}

// New returns the participant. exec runs the local drain pass; cache may be
// nil while this process holds no session cache. now is the clock.
func New(js jetstream.KeyValueManager, terminating admission.TerminationChecker, exec *executor.Executor,
	cache executor.SessionCache, now func() time.Time,
) (*Participant, error) {
	if js == nil || terminating == nil || exec == nil {
		return nil, errors.New("migration participant needs JetStream, a termination checker and an executor")
	}
	if now == nil {
		now = time.Now
	}
	return &Participant{
		js: js, terminating: terminating, seals: admission.NewSeals(js),
		exec: exec, cache: cache, now: now,
	}, nil
}

func checkAccount(accountID string) error {
	if !awsidentifiers.IsAccountID(accountID) || accountID == awsidentifiers.GlobalAccountID {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%q is not a tenant account ID", accountID)
	}
	return nil
}

// Inventory reports every owned class for the account from the account
// bucket. A bucket that still exists is not clean, even when it is empty,
// because the command removes the objects before the bucket.
func (p *Participant) Inventory(ctx context.Context, req *accountlifecyclev1.InventoryRequest, _ string) (*accountlifecyclev1.InventoryReply, error) {
	if err := checkAccount(req.AccountID); err != nil {
		return nil, err
	}
	classes := p.read(ctx, req.AccountID)
	return &accountlifecyclev1.InventoryReply{
		AccountID: req.AccountID, Participant: migrationv1.ParticipantName,
		DeclarationVersion: migrationv1.DeclarationVersion, ContractVersion: accountlifecyclev1.ContractVersion,
		Classes: classes,
	}, nil
}

// Terminate runs one pass: seal, mark every record for deletion, drain what no
// executor holds, then remove the objects and the bucket once no record is
// left. It returns without waiting for an owner; the engine repeats it.
func (p *Participant) Terminate(ctx context.Context, req *accountlifecyclev1.TerminateRequest, _ string) (*accountlifecyclev1.TerminateReply, error) {
	if err := checkAccount(req.AccountID); err != nil {
		return nil, err
	}
	account := req.AccountID
	reply := &accountlifecyclev1.TerminateReply{
		AccountID: account, Participant: migrationv1.ParticipantName,
		DeclarationVersion: migrationv1.DeclarationVersion, ContractVersion: accountlifecyclev1.ContractVersion,
	}

	if err := p.seals.Seal(ctx, p.terminating, account, p.now()); err != nil {
		if errors.Is(err, admission.ErrNotTerminating) {
			return nil, awserrors.Errorf(awserrors.ErrorIncorrectState, "account %s is not TERMINATING", account)
		}
		return nil, err
	}
	if p.cache != nil {
		p.cache.DropAccount(account)
	}

	waiting, err := p.exec.Retire(ctx, account, p.now())
	for _, kind := range store.Kinds() {
		if n := waiting[kind]; n > 0 {
			reply.Blocked = append(reply.Blocked, accountlifecyclev1.BlockedItem{
				Class: kind, Reason: accountlifecyclev1.ReasonOwnerNotStopped, Count: n,
			})
		}
	}
	if err != nil {
		// A failed pass is never clean, even when the bucket is already gone.
		slog.WarnContext(ctx, "Migration termination pass incomplete", "accountID", account, "err", err)
		reply.Classes = allClasses(accountlifecyclev1.StatusIncomplete, accountlifecyclev1.ReasonStoreError)
		return reply, nil
	}
	reply.Classes = p.read(ctx, account)
	return reply, nil
}

// read builds the class reports. A store failure, an unreadable record or a
// key of no declared kind makes a class incomplete, never clean.
func (p *Participant) read(ctx context.Context, account string) []accountlifecyclev1.ClassReport {
	all := allClasses

	s, err := store.Open(ctx, p.js, account)
	if errors.Is(err, store.ErrNotFound) {
		return all(accountlifecyclev1.StatusClean)
	}
	if err != nil {
		return all(accountlifecyclev1.StatusIncomplete, accountlifecyclev1.ReasonStoreError)
	}
	listing, err := s.List(ctx)
	if err != nil {
		return all(accountlifecyclev1.StatusIncomplete, accountlifecyclev1.ReasonStoreError)
	}
	if listing.Unknown > 0 {
		return all(accountlifecyclev1.StatusIncomplete, accountlifecyclev1.ReasonUnreadableRecord)
	}

	kinds := store.Kinds()
	out := make([]accountlifecyclev1.ClassReport, 0, len(kinds))
	counts := map[string]int{}
	for _, e := range listing.Entries {
		counts[e.Record.Kind]++
	}
	for _, kind := range kinds {
		r := accountlifecyclev1.ClassReport{Class: kind, Remaining: counts[kind], Status: accountlifecyclev1.StatusPresent}
		switch {
		case listing.Unreadable[kind] > 0:
			r.Status, r.Reasons = accountlifecyclev1.StatusIncomplete, []string{accountlifecyclev1.ReasonUnreadableRecord}
		case counts[kind] == 0:
			r.Reasons = []string{accountlifecyclev1.ReasonDraining}
		}
		out = append(out, r)
	}
	return out
}

// allClasses reports every owned class with one status.
func allClasses(status string, reasons ...string) []accountlifecyclev1.ClassReport {
	kinds := store.Kinds()
	out := make([]accountlifecyclev1.ClassReport, 0, len(kinds))
	for _, kind := range kinds {
		out = append(out, accountlifecyclev1.ClassReport{Class: kind, Status: status, Reasons: reasons})
	}
	return out
}
