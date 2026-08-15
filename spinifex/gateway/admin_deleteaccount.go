package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mulgadc/spinifex/spinifex/accountteardown"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// KVBucketAccountDeletions holds one deletion job per account, keyed by account
// ID. Keying by account rather than by deletion ID is deliberate: an account is
// deleted once, and the record is what keeps "what happened to account X"
// answerable after the account itself is gone.
const KVBucketAccountDeletions = "spinifex-account-deletions"

// accountDeletionTimeout is the outer bound on a whole teardown. It must stay
// well above the sum of the engine's per-stage drain budgets: the stages are
// what decide when to give up on a resource, and this cutting in first would
// abandon a teardown midway with no stuck report to show for it.
const accountDeletionTimeout = 2 * time.Hour

// accountDeletionHeartbeat is how often a running job refreshes its record. A
// job whose heartbeat has stopped for accountDeletionStale is assumed dead —
// the gateway running it was restarted — and another gateway may take it over.
const (
	accountDeletionHeartbeat = 30 * time.Second
	accountDeletionStale     = 5 * time.Minute
)

// accountDeletionInventoryTimeout bounds a dry run, which only lists. It is
// answered inline, so it must not hold an HTTP request for minutes.
const accountDeletionInventoryTimeout = 2 * time.Minute

// Deletion job states.
const (
	DeletionStateRunning   = "RUNNING"
	DeletionStateCompleted = "COMPLETED"
	DeletionStateFailed    = "FAILED"
)

var accountIDRE = regexp.MustCompile(`^[0-9]{12}$`)

// DeleteAccountRequest is the /admin/DeleteAccount request body.
type DeleteAccountRequest struct {
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	ClientToken string `json:"clientToken"`
	Force       bool   `json:"force,omitempty"`
	DryRun      bool   `json:"dryRun,omitempty"`
}

// DeleteAccountResponse acknowledges a started (or replayed) teardown. The
// teardown itself runs in the background: it takes minutes, and holding the
// request open for it would make every retry start a second one.
type DeleteAccountResponse struct {
	DeletionID string                  `json:"deletionId"`
	AccountID  string                  `json:"accountId"`
	State      string                  `json:"state"`
	DryRun     bool                    `json:"dryRun"`
	Inventory  *accountteardown.Result `json:"inventory,omitempty"`
}

// DescribeAccountDeletionRequest is the /admin/DescribeAccountDeletion body.
type DescribeAccountDeletionRequest struct {
	AccountID string `json:"accountId"`
}

// accountDeletionJob is the stored progress of one teardown. It is written
// before the teardown starts and updated after every stage, so a crash leaves
// an attributable record of how far it got.
type accountDeletionJob struct {
	DeletionID  string `json:"deletionId"`
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	ClientToken string `json:"clientToken"`
	State       string `json:"state"`
	Force       bool   `json:"force"`
	StartedAt   string `json:"startedAt"`
	// UpdatedAt is the running job's heartbeat. Its absence for long enough is
	// what lets another gateway take over after a restart.
	UpdatedAt  string                        `json:"updatedAt"`
	FinishedAt string                        `json:"finishedAt,omitempty"`
	Stages     []accountteardown.StageResult `json:"stages,omitempty"`
	// Error is the failure reason, including the stuck-resource summary. The
	// per-stage Stuck entries carry which resources and why.
	Error string `json:"error,omitempty"`
}

// adminDeleteAccount starts a teardown, or answers a dry run inline.
func (gw *GatewayConfig) adminDeleteAccount(ctx context.Context, body []byte) (any, error) {
	var req DeleteAccountRequest
	if err := json.Unmarshal(body, &req); err != nil {
		slog.Debug("DeleteAccount: malformed JSON body", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidRequest)
	}
	if err := validateDeleteAccountRequest(&req); err != nil {
		return nil, err
	}
	if gw.IAMService == nil {
		slog.Error("DeleteAccount: IAM service not available")
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	engine, err := accountteardown.NewClusterEngine(ctx, gw.NATSConn, gw.ExpectedNodes, gw.IAMService)
	if err != nil {
		slog.Error("DeleteAccount: failed to build teardown engine", "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailable)
	}

	if req.DryRun {
		inventoryCtx, cancel := context.WithTimeout(ctx, accountDeletionInventoryTimeout)
		defer cancel()

		inventory, err := engine.Inventory(inventoryCtx, req.AccountID)
		if err != nil {
			return nil, teardownError("DeleteAccount", req.AccountID, err)
		}
		return &DeleteAccountResponse{
			AccountID: req.AccountID, State: "DRY_RUN", DryRun: true, Inventory: inventory,
		}, nil
	}

	jobs, err := gw.accountDeletionStore(ctx)
	if err != nil {
		return nil, err
	}

	// Confirm the account is deletable before claiming the job, so a mistyped
	// id or a protected account does not leave a job record behind.
	if _, err := engine.Inventory(ctx, req.AccountID); err != nil {
		return nil, teardownError("DeleteAccount", req.AccountID, err)
	}

	job, replay, err := claimDeletionJob(ctx, jobs, &req)
	if err != nil {
		return nil, err
	}
	if replay != nil {
		slog.Info("DeleteAccount: replayed existing job",
			"accountID", replay.AccountID, "deletionID", replay.DeletionID, "state", replay.State)
		return &DeleteAccountResponse{
			DeletionID: replay.DeletionID, AccountID: replay.AccountID, State: replay.State,
		}, nil
	}

	gw.runAccountDeletion(engine, jobs, job)

	return &DeleteAccountResponse{
		DeletionID: job.DeletionID, AccountID: job.AccountID, State: job.State,
	}, nil
}

// adminDescribeAccountDeletion returns the stored job for an account, including
// one that finished — the record outlives the account it describes.
func (gw *GatewayConfig) adminDescribeAccountDeletion(ctx context.Context, body []byte) (any, error) {
	var req DescribeAccountDeletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		slog.Debug("DescribeAccountDeletion: malformed JSON body", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidRequest)
	}
	req.AccountID = strings.TrimSpace(req.AccountID)
	if req.AccountID == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if !accountIDRE.MatchString(req.AccountID) {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	jobs, err := gw.accountDeletionStore(ctx)
	if err != nil {
		return nil, err
	}

	job, err := readDeletionJob(ctx, jobs, req.AccountID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return job, nil
}

// AccountSummary is one row of /admin/ListAccounts. It carries no credential
// material: the caller can already create and delete accounts, but that is no
// reason for a listing to hand out keys.
type AccountSummary struct {
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	Status      string `json:"status"`
	CreatedAt   string `json:"createdAt,omitempty"`
}

// ListAccountsResponse is the /admin/ListAccounts success body.
type ListAccountsResponse struct {
	Accounts []AccountSummary `json:"accounts"`
	Count    int              `json:"count"`
}

// adminListAccounts enumerates every account. It takes no parameters: the
// account cap bounds the listing, so paging would be complexity with no payer.
func (gw *GatewayConfig) adminListAccounts(_ context.Context, _ []byte) (any, error) {
	if gw.IAMService == nil {
		slog.Error("ListAccounts: IAM service not available")
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	accounts, err := gw.IAMService.ListAccounts()
	if err != nil {
		slog.Error("ListAccounts: failed to list accounts", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	response := &ListAccountsResponse{Accounts: make([]AccountSummary, 0, len(accounts))}
	for _, account := range accounts {
		if account == nil {
			continue
		}
		response.Accounts = append(response.Accounts, AccountSummary{
			AccountID:   account.AccountID,
			AccountName: account.AccountName,
			Status:      account.Status,
			CreatedAt:   account.CreatedAt,
		})
	}
	response.Count = len(response.Accounts)
	return response, nil
}

// runAccountDeletion runs the teardown in the background on its own context.
// The request's context is cancelled the moment the response is written, and a
// teardown cancelled halfway leaves the account half-emptied.
func (gw *GatewayConfig) runAccountDeletion(
	engine *accountteardown.Engine,
	jobs jetstream.KeyValue,
	job *accountDeletionJob,
) {
	tracker := &deletionTracker{jobs: jobs, job: job}
	engine.OnStage = tracker.stageFinished

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), accountDeletionTimeout)
		defer cancel()

		stop := tracker.startHeartbeat(ctx)
		defer stop()

		result, err := engine.Teardown(ctx, accountteardown.Request{
			AccountID:   job.AccountID,
			AccountName: job.AccountName,
			Force:       job.Force,
		})
		tracker.finished(ctx, result, err)
	}()
}

// deletionTracker keeps the stored job in step with a teardown as it runs. Its
// mutex covers the heartbeat and the stage callback, which write concurrently.
type deletionTracker struct {
	jobs jetstream.KeyValue

	mu  sync.Mutex
	job *accountDeletionJob
}

func (t *deletionTracker) stageFinished(ctx context.Context, stage accountteardown.StageResult) {
	t.mu.Lock()
	t.job.Stages = append(t.job.Stages, stage)
	t.mu.Unlock()
	t.persist(ctx)
}

func (t *deletionTracker) finished(ctx context.Context, result *accountteardown.Result, err error) {
	t.mu.Lock()
	if result != nil {
		t.job.Stages = result.Stages
	}
	if err != nil {
		t.job.State = DeletionStateFailed
		t.job.Error = err.Error()
	} else {
		t.job.State = DeletionStateCompleted
	}
	t.job.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	accountID, state := t.job.AccountID, t.job.State
	t.mu.Unlock()

	t.persist(ctx)
	if err != nil {
		slog.Error("DeleteAccount: teardown failed", "accountID", accountID, "err", err)
		return
	}
	slog.Info("DeleteAccount: teardown complete", "accountID", accountID, "state", state)
}

// startHeartbeat refreshes UpdatedAt while the teardown runs. Without it a
// gateway restarted mid-teardown leaves a job that looks live forever, and the
// account can never be retried through the API.
func (t *deletionTracker) startHeartbeat(ctx context.Context) func() {
	ticker := time.NewTicker(accountDeletionHeartbeat)
	done := make(chan struct{})

	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				t.persist(ctx)
			}
		}
	}()

	return func() { close(done) }
}

func (t *deletionTracker) persist(ctx context.Context) {
	t.mu.Lock()
	t.job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	record, err := json.Marshal(t.job)
	accountID := t.job.AccountID
	t.mu.Unlock()

	if err != nil {
		slog.Error("DeleteAccount: failed to marshal job record", "accountID", accountID, "err", err)
		return
	}
	if _, err := t.jobs.Put(ctx, accountID, record); err != nil {
		// The teardown itself is unaffected; only its progress report is. Losing
		// the run over a KV write would be the worse outcome.
		slog.Error("DeleteAccount: failed to store job progress", "accountID", accountID, "err", err)
	}
}

// claimDeletionJob CAS-creates the job record, which is what stops two
// gateways tearing the same account down at once. It returns a non-nil replay
// when an existing job answers the request instead.
func claimDeletionJob(
	ctx context.Context,
	jobs jetstream.KeyValue,
	req *DeleteAccountRequest,
) (*accountDeletionJob, *accountDeletionJob, error) {
	now := time.Now().UTC()
	job := &accountDeletionJob{
		DeletionID:  uuid.NewString(),
		AccountID:   req.AccountID,
		AccountName: req.AccountName,
		ClientToken: req.ClientToken,
		State:       DeletionStateRunning,
		Force:       req.Force,
		StartedAt:   now.Format(time.RFC3339),
		UpdatedAt:   now.Format(time.RFC3339),
	}
	record, err := json.Marshal(job)
	if err != nil {
		slog.Error("DeleteAccount: failed to marshal job record", "err", err)
		return nil, nil, errors.New(awserrors.ErrorInternalError)
	}

	if _, err := jobs.Create(ctx, req.AccountID, record); err == nil {
		return job, nil, nil
	} else if !errors.Is(err, jetstream.ErrKeyExists) {
		slog.Error("DeleteAccount: failed to claim deletion job", "accountID", req.AccountID, "err", err)
		return nil, nil, errors.New(awserrors.ErrorInternalError)
	}

	existing, err := readDeletionJob(ctx, jobs, req.AccountID)
	if err != nil {
		return nil, nil, err
	}
	if existing == nil {
		// Deleted between Create and Get. Retryable rather than a fault.
		return nil, nil, errors.New(awserrors.ErrorOperationInProgress)
	}

	// A token bound to a different account name is a client bug, not a retry.
	if existing.ClientToken == req.ClientToken {
		if existing.AccountName != req.AccountName {
			return nil, nil, errors.New(awserrors.ErrorIdempotentParameterMismatch)
		}
		return nil, existing, nil
	}
	if existing.State == DeletionStateRunning && !deletionJobStale(existing, now) {
		return nil, nil, errors.New(awserrors.ErrorOperationInProgress)
	}
	if existing.State == DeletionStateCompleted {
		// The account is gone. Saying so is more useful than starting a second
		// teardown that would fail on a missing account record.
		return nil, existing, nil
	}

	// A failed job, or one whose gateway stopped heartbeating, is retried under
	// a new deletion id. Teardown is idempotent: it re-lists what is left.
	if _, err := jobs.Put(ctx, req.AccountID, record); err != nil {
		slog.Error("DeleteAccount: failed to take over deletion job", "accountID", req.AccountID, "err", err)
		return nil, nil, errors.New(awserrors.ErrorInternalError)
	}
	slog.Warn("DeleteAccount: retrying a job that did not finish",
		"accountID", req.AccountID, "previousDeletionID", existing.DeletionID, "previousState", existing.State)
	return job, nil, nil
}

// deletionJobStale reports whether a RUNNING job has stopped heartbeating for
// long enough that the gateway running it is presumed gone.
func deletionJobStale(job *accountDeletionJob, now time.Time) bool {
	updated, err := time.Parse(time.RFC3339, job.UpdatedAt)
	if err != nil {
		// An unparseable heartbeat cannot prove the job is alive, and refusing
		// forever on a malformed record is worse than allowing a retry.
		return true
	}
	return now.Sub(updated) > accountDeletionStale
}

func readDeletionJob(ctx context.Context, jobs jetstream.KeyValue, accountID string) (*accountDeletionJob, error) {
	entry, err := jobs.Get(ctx, accountID)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, nil
		}
		slog.Error("DeleteAccount: failed to read deletion job", "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	var job accountDeletionJob
	if err := json.Unmarshal(entry.Value(), &job); err != nil {
		slog.Error("DeleteAccount: failed to unmarshal deletion job", "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	return &job, nil
}

// accountDeletionStore opens the job bucket. Opened per request, like the
// CreateAccount stores, so a cluster upgraded into this feature picks it up
// without a gateway restart.
func (gw *GatewayConfig) accountDeletionStore(ctx context.Context) (jetstream.KeyValue, error) {
	js, err := jetstream.New(gw.NATSConn)
	if err != nil {
		slog.Error("DeleteAccount: failed to get JetStream context", "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailable)
	}

	jobs, err := kvutil.GetOrCreateBucket(ctx, js, KVBucketAccountDeletions, 5)
	if err != nil {
		slog.Error("DeleteAccount: failed to open deletion job bucket", "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailable)
	}
	return jobs, nil
}

// teardownError maps an engine error onto the admin surface.
//
// A protected account returns AccessDenied rather than a description of why:
// no credential grants deleting it, so the answer is the same one an
// unauthorized caller gets.
func teardownError(method, accountID string, err error) error {
	switch {
	case errors.Is(err, accountteardown.ErrProtectedAccount):
		slog.Warn(method+": refused a protected account", "accountID", accountID)
		return errors.New(awserrors.ErrorAccessDenied)
	case errors.Is(err, accountteardown.ErrAccountNameMismatch):
		slog.Warn(method+": account name did not match", "accountID", accountID)
		return errors.New(awserrors.ErrorIdempotentParameterMismatch)
	case isAccountNotFound(err):
		return errors.New(awserrors.ErrorIAMNoSuchEntity)
	default:
		slog.Error(method+": teardown could not start", "accountID", accountID, "err", err)
		return errors.New(awserrors.ErrorInternalError)
	}
}

// isAccountNotFound recognises the IAM service's missing-account error, which
// is a plain error value rather than a typed one.
func isAccountNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "account not found")
}

// validateDeleteAccountRequest rejects malformed input. Messages name the
// offending field but never echo its value, which for accountName is a
// customer email address.
func validateDeleteAccountRequest(req *DeleteAccountRequest) error {
	req.AccountID = strings.TrimSpace(req.AccountID)
	req.AccountName = strings.TrimSpace(req.AccountName)
	req.ClientToken = strings.TrimSpace(req.ClientToken)

	if req.AccountID == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if !accountIDRE.MatchString(req.AccountID) {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if req.DryRun {
		// A dry run deletes nothing, so it needs neither the name confirmation
		// nor a token to make it idempotent.
		return nil
	}

	// The name confirmation is the safety rail that makes a mistyped account id
	// fail closed rather than empty a live tenant.
	if req.AccountName == "" || req.ClientToken == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if len(req.AccountName) > accountNameMaxLen {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if len(req.ClientToken) < clientTokenMinLen || len(req.ClientToken) > clientTokenMaxLen ||
		!clientTokenRE.MatchString(req.ClientToken) {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return nil
}
