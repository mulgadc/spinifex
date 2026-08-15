package accountteardown

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

const (
	testAccountID   = "000000000042"
	testAccountName = "tenant@example.com"
)

// fakeAccounts is an in-memory AccountStore that records what happened to it.
type fakeAccounts struct {
	account   *Account
	statuses  []string
	deleted   bool
	getErr    error
	statusErr error
	deleteErr error
}

func newFakeAccounts() *fakeAccounts {
	return &fakeAccounts{account: &Account{
		AccountID:   testAccountID,
		AccountName: testAccountName,
		Status:      "ACTIVE",
	}}
}

func (f *fakeAccounts) GetAccount(string) (*Account, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.account, nil
}

func (f *fakeAccounts) SetAccountStatus(_, status string) (*Account, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	f.statuses = append(f.statuses, status)
	f.account.Status = status
	return f.account, nil
}

func (f *fakeAccounts) DeleteAccount(string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = true
	return nil
}

// fakeReaper holds a set of resources and removes them when deleted. order is
// shared between reapers so a test can assert the sequence across kinds.
type fakeReaper struct {
	kind      string
	stage     Stage
	resources []Resource
	order     *[]string

	// blockedUntilForce refuses to delete anything unless force is set, which
	// is how the attached-volume deadlock is modelled.
	blockedUntilForce bool

	// listErr, when set, fails enumeration.
	listErr error

	// deletesAttempted counts every Delete call, successful or not.
	deletesAttempted int
}

func (r *fakeReaper) Kind() string { return r.kind }
func (r *fakeReaper) Stage() Stage { return r.stage }

func (r *fakeReaper) List(context.Context, string) ([]Resource, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return append([]Resource(nil), r.resources...), nil
}

func (r *fakeReaper) Delete(_ context.Context, _ string, resource Resource, force bool) error {
	r.deletesAttempted++
	if r.blockedUntilForce && !force {
		return errors.New("resource is in use")
	}
	if r.order != nil {
		*r.order = append(*r.order, resource.Kind+"/"+resource.ID)
	}
	remaining := r.resources[:0]
	for _, held := range r.resources {
		if held.ID != resource.ID {
			remaining = append(remaining, held)
		}
	}
	r.resources = remaining
	return nil
}

func quietEngine(accounts AccountStore, reapers ...Reaper) *Engine {
	engine := NewEngine(accounts, reapers...)
	engine.Logger = slog.New(slog.DiscardHandler)
	engine.Timeouts = Timeouts{StageDrain: 200 * time.Millisecond, DrainPoll: 5 * time.Millisecond}
	return engine
}

func request() Request {
	return Request{AccountID: testAccountID, AccountName: testAccountName}
}

// The protected-account list must be the same two accounts the rest of the
// system treats as undeletable. Two hard-coded lists that drift is exactly the
// failure this guards against.
func TestProtectedAccountsMatchTheRestOfTheSystem(t *testing.T) {
	for _, id := range []string{admin.SystemAccountID(), admin.DefaultAccountID()} {
		if _, protected := protectedAccountIDs[id]; !protected {
			t.Fatalf("account %s is not protected by teardown", id)
		}
		if _, protected := handlers_iam.UndeletableAccountIDs[id]; !protected {
			t.Fatalf("account %s is not protected by the IAM service", id)
		}
	}
	if len(protectedAccountIDs) != len(handlers_iam.UndeletableAccountIDs) {
		t.Fatal("teardown and the IAM service disagree on which accounts are protected")
	}
}

func TestTeardownRefusesTheSuperAdminAccount(t *testing.T) {
	accounts := newFakeAccounts()
	engine := quietEngine(accounts)

	_, err := engine.Teardown(context.Background(), Request{
		AccountID: admin.DefaultAccountID(), AccountName: "spinifex",
	})
	if !errors.Is(err, ErrProtectedAccount) {
		t.Fatalf("expected ErrProtectedAccount, got %v", err)
	}
	if len(accounts.statuses) != 0 {
		t.Fatal("a protected account must not even be marked terminating")
	}
}

func TestTeardownRefusesTheSystemAccount(t *testing.T) {
	engine := quietEngine(newFakeAccounts())

	_, err := engine.Teardown(context.Background(), Request{
		AccountID: admin.SystemAccountID(), AccountName: "system",
	})
	if !errors.Is(err, ErrProtectedAccount) {
		t.Fatalf("expected ErrProtectedAccount, got %v", err)
	}
}

// A mistyped account id must fail closed rather than empty a live tenant.
func TestTeardownRefusesAMismatchedName(t *testing.T) {
	accounts := newFakeAccounts()
	reaper := &fakeReaper{kind: "volume", stage: StageStorage, resources: []Resource{{Kind: "volume", ID: "vol-1"}}}
	engine := quietEngine(accounts, reaper)

	_, err := engine.Teardown(context.Background(), Request{
		AccountID: testAccountID, AccountName: "someone-else@example.com",
	})
	if !errors.Is(err, ErrAccountNameMismatch) {
		t.Fatalf("expected ErrAccountNameMismatch, got %v", err)
	}
	if reaper.deletesAttempted != 0 {
		t.Fatal("nothing may be deleted when the name does not match")
	}
	if accounts.deleted {
		t.Fatal("the account must survive a name mismatch")
	}
}

// The error must not disclose the stored name: it is a customer email address
// and a caller who guessed an id would otherwise learn it by probing.
func TestNameMismatchDoesNotDiscloseTheStoredName(t *testing.T) {
	engine := quietEngine(newFakeAccounts())

	_, err := engine.Teardown(context.Background(), Request{
		AccountID: testAccountID, AccountName: "guess@example.com",
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testAccountName) {
		t.Fatalf("error disclosed the stored account name: %v", err)
	}
}

// The account is quiesced before anything is deleted, otherwise the customer
// keeps creating resources behind the cascade and it never converges.
func TestTeardownMarksTerminatingBeforeDeletingAnything(t *testing.T) {
	accounts := newFakeAccounts()
	var order []string
	reaper := &fakeReaper{
		kind: "volume", stage: StageStorage, order: &order,
		resources: []Resource{{Kind: "volume", ID: "vol-1"}},
	}
	engine := quietEngine(accounts, reaper)

	if _, err := engine.Teardown(context.Background(), request()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if len(accounts.statuses) == 0 || accounts.statuses[0] != AccountStatusTerminating {
		t.Fatalf("expected TERMINATING to be set first, got %v", accounts.statuses)
	}
	if !accounts.deleted {
		t.Fatal("a fully drained account must be deleted")
	}
}

// Stage order is the reason teardown converges at all: an instance holding a
// volume has to go first, or the volume is undeletable.
func TestTeardownRunsStagesInDependencyOrder(t *testing.T) {
	var order []string
	volumes := &fakeReaper{kind: "volume", stage: StageStorage, order: &order,
		resources: []Resource{{Kind: "volume", ID: "vol-1"}}}
	instances := &fakeReaper{kind: "instance", stage: StageCompute, order: &order,
		resources: []Resource{{Kind: "instance", ID: "i-1"}}}
	vpcs := &fakeReaper{kind: "vpc", stage: StageNetwork, order: &order,
		resources: []Resource{{Kind: "vpc", ID: "vpc-1"}}}

	// Registered out of order on purpose: the stage decides, not the caller.
	engine := quietEngine(newFakeAccounts(), vpcs, volumes, instances)
	if _, err := engine.Teardown(context.Background(), request()); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	want := []string{"instance/i-1", "volume/vol-1", "vpc/vpc-1"}
	if len(order) != len(want) {
		t.Fatalf("expected %v, got %v", want, order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, order)
		}
	}
}

// Within one stage the registration order carries the dependency, which is
// where snapshots-before-volumes lives.
func TestTeardownPreservesRegistrationOrderWithinAStage(t *testing.T) {
	var order []string
	snapshots := &fakeReaper{kind: "snapshot", stage: StageStorage, order: &order,
		resources: []Resource{{Kind: "snapshot", ID: "snap-1"}}}
	volumes := &fakeReaper{kind: "volume", stage: StageStorage, order: &order,
		resources: []Resource{{Kind: "volume", ID: "vol-1"}}}

	engine := quietEngine(newFakeAccounts(), snapshots, volumes)
	if _, err := engine.Teardown(context.Background(), request()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if len(order) != 2 || order[0] != "snapshot/snap-1" {
		t.Fatalf("snapshots must be deleted before volumes, got %v", order)
	}
}

// The deadlock the whole --force flag exists for: a volume an instance still
// holds. Without force it must be reported, not silently skipped.
func TestStuckResourceIsReportedAndTheAccountSurvives(t *testing.T) {
	accounts := newFakeAccounts()
	volumes := &fakeReaper{
		kind: "volume", stage: StageStorage, blockedUntilForce: true,
		resources: []Resource{{Kind: "volume", ID: "vol-stuck", Detail: "attached to i-1"}},
	}
	engine := quietEngine(accounts, volumes)

	result, err := engine.Teardown(context.Background(), request())
	if !errors.Is(err, ErrResourcesStuck) {
		t.Fatalf("expected ErrResourcesStuck, got %v", err)
	}
	if result.StuckCount() != 1 {
		t.Fatalf("expected 1 stuck resource, got %d", result.StuckCount())
	}
	if accounts.deleted {
		t.Fatal("the account must stay so the residue keeps an owner")
	}
	if accounts.account.Status != AccountStatusTerminating {
		t.Fatalf("a failed teardown must leave the account TERMINATING, got %q", accounts.account.Status)
	}
	if result.AccountDeleted {
		t.Fatal("AccountDeleted must be false when anything was left behind")
	}
}

// The reason a resource would not go is the useful half of the report.
func TestStuckResourceCarriesTheDeleteFailureReason(t *testing.T) {
	volumes := &fakeReaper{
		kind: "volume", stage: StageStorage, blockedUntilForce: true,
		resources: []Resource{{Kind: "volume", ID: "vol-stuck"}},
	}
	engine := quietEngine(newFakeAccounts(), volumes)

	result, _ := engine.Teardown(context.Background(), request())
	if result.StuckCount() != 1 {
		t.Fatalf("expected 1 stuck resource, got %d", result.StuckCount())
	}
	for _, stage := range result.Stages {
		for _, stuck := range stage.Stuck {
			if !strings.Contains(stuck.Reason, "in use") {
				t.Fatalf("expected the delete failure as the reason, got %q", stuck.Reason)
			}
		}
	}
}

func TestForceClearsAResourceThatRefusesTheOrdinaryDelete(t *testing.T) {
	accounts := newFakeAccounts()
	volumes := &fakeReaper{
		kind: "volume", stage: StageStorage, blockedUntilForce: true,
		resources: []Resource{{Kind: "volume", ID: "vol-stuck", Detail: "attached to i-1"}},
	}
	engine := quietEngine(accounts, volumes)

	req := request()
	req.Force = true
	result, err := engine.Teardown(context.Background(), req)
	if err != nil {
		t.Fatalf("force teardown: %v", err)
	}
	if result.StuckCount() != 0 {
		t.Fatalf("force must clear the stuck volume, got %d stuck", result.StuckCount())
	}
	if !accounts.deleted {
		t.Fatal("a fully drained account must be deleted")
	}
}

// A dry run is the pre-flight for every runbook step and for the load-test
// harness, so it must be provably read-only.
func TestDryRunDeletesNothing(t *testing.T) {
	accounts := newFakeAccounts()
	volumes := &fakeReaper{kind: "volume", stage: StageStorage,
		resources: []Resource{{Kind: "volume", ID: "vol-1"}}}
	instances := &fakeReaper{kind: "instance", stage: StageCompute,
		resources: []Resource{{Kind: "instance", ID: "i-1"}}}
	engine := quietEngine(accounts, volumes, instances)

	req := request()
	req.DryRun = true
	result, err := engine.Teardown(context.Background(), req)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if volumes.deletesAttempted != 0 || instances.deletesAttempted != 0 {
		t.Fatal("a dry run must not attempt any delete")
	}
	if len(accounts.statuses) != 0 {
		t.Fatal("a dry run must not change the account status")
	}
	if accounts.deleted {
		t.Fatal("a dry run must not delete the account")
	}
	if result.DeletedCount() != 2 {
		t.Fatalf("expected both resources in the inventory, got %d", result.DeletedCount())
	}
}

// Inventory is the read-only probe, so it must not require the confirmation
// name and must not touch the account.
func TestInventoryIsReadOnlyAndNeedsNoConfirmation(t *testing.T) {
	accounts := newFakeAccounts()
	volumes := &fakeReaper{kind: "volume", stage: StageStorage,
		resources: []Resource{{Kind: "volume", ID: "vol-1"}}}
	engine := quietEngine(accounts, volumes)

	result, err := engine.Inventory(context.Background(), testAccountID)
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if result.DeletedCount() != 1 {
		t.Fatalf("expected 1 resource, got %d", result.DeletedCount())
	}
	if volumes.deletesAttempted != 0 || len(accounts.statuses) != 0 || accounts.deleted {
		t.Fatal("inventory must not change anything")
	}
}

func TestInventoryRefusesAProtectedAccount(t *testing.T) {
	engine := quietEngine(newFakeAccounts())

	if _, err := engine.Inventory(context.Background(), admin.SystemAccountID()); !errors.Is(err, ErrProtectedAccount) {
		t.Fatalf("expected ErrProtectedAccount, got %v", err)
	}
}

// A listing that fails says nothing about whether the account is empty, so it
// must stop the teardown rather than let it read as drained.
func TestAFailedListingAbortsTheTeardown(t *testing.T) {
	accounts := newFakeAccounts()
	volumes := &fakeReaper{kind: "volume", stage: StageStorage, listErr: errors.New("store unavailable")}
	engine := quietEngine(accounts, volumes)

	if _, err := engine.Teardown(context.Background(), request()); err == nil {
		t.Fatal("expected the failed listing to abort the teardown")
	}
	if accounts.deleted {
		t.Fatal("the account must not be deleted when a listing failed")
	}
}

// Teardown re-runs after a crash, so a second pass over an empty account has
// to succeed rather than trip over resources that are already gone.
func TestTeardownIsIdempotentOverAnAlreadyEmptyAccount(t *testing.T) {
	accounts := newFakeAccounts()
	volumes := &fakeReaper{kind: "volume", stage: StageStorage}
	engine := quietEngine(accounts, volumes)

	result, err := engine.Teardown(context.Background(), request())
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if result.DeletedCount() != 0 || !result.AccountDeleted {
		t.Fatalf("expected an empty successful teardown, got %+v", result)
	}
}

// A stage whose deletes are asynchronous is finished when a fresh listing is
// empty, not when the delete calls returned.
func TestAStageWaitsForItsResourcesToActuallyDisappear(t *testing.T) {
	slow := &slowReaper{kind: "instance", stage: StageCompute, listsBeforeGone: 3,
		resource: Resource{Kind: "instance", ID: "i-1"}}
	engine := quietEngine(newFakeAccounts(), slow)

	result, err := engine.Teardown(context.Background(), request())
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if result.StuckCount() != 0 {
		t.Fatalf("expected the instance to drain, got %d stuck", result.StuckCount())
	}
	if slow.lists < 2 {
		t.Fatalf("expected the stage to re-list until empty, listed %d times", slow.lists)
	}
}

// slowReaper keeps reporting its resource for a few listings after the delete
// is accepted, the way a terminate does.
type slowReaper struct {
	kind            string
	stage           Stage
	resource        Resource
	listsBeforeGone int
	lists           int
	deleted         bool
}

func (r *slowReaper) Kind() string { return r.kind }
func (r *slowReaper) Stage() Stage { return r.stage }

func (r *slowReaper) List(context.Context, string) ([]Resource, error) {
	r.lists++
	if r.deleted && r.lists > r.listsBeforeGone {
		return nil, nil
	}
	return []Resource{r.resource}, nil
}

func (r *slowReaper) Delete(context.Context, string, Resource, bool) error {
	r.deleted = true
	return nil
}
