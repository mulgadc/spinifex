package accountlifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	admissionv1 "github.com/mulgadc/spinifex/contracts/admission/v1"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

// quotaOverrideSealWait bounds one seal request; an earlier caller deadline wins.
const quotaOverrideSealWait = 30 * time.Second

// quotaOverrideSealer is the quota owner's seal of one account's override
// record.
type quotaOverrideSealer interface {
	SealAccountQuotaOverride(ctx context.Context, accountID string) (revision uint64, err error)
}

// quotaOverrideSealClient reaches the quota override seal contract over NATS.
// Only a reply for this account, disposition sealed and a non-zero revision is
// a seal; anything else, including no responder, is an error.
type quotaOverrideSealClient struct {
	nc   *nats.Conn
	wait time.Duration
}

var _ quotaOverrideSealer = (*quotaOverrideSealClient)(nil)

func newQuotaOverrideSealClient(nc *nats.Conn) *quotaOverrideSealClient {
	return &quotaOverrideSealClient{nc: nc, wait: quotaOverrideSealWait}
}

func (c *quotaOverrideSealClient) SealAccountQuotaOverride(ctx context.Context, accountID string) (uint64, error) {
	wait := c.wait
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(deadline))
	}
	if ctx.Err() != nil || wait <= 0 {
		return 0, fmt.Errorf("seal quota override: %w", context.DeadlineExceeded)
	}

	reply, err := natsmsg.NATSRequest[admissionv1.SealAccountQuotaOverrideReply](ctx, c.nc, admissionv1.SealAccountQuotaOverrideSubject,
		&admissionv1.SealAccountQuotaOverrideRequest{AccountID: accountID}, wait, accountID)
	if err != nil {
		return 0, fmt.Errorf("seal quota override: %w", err)
	}
	if reply.AccountID != accountID {
		return 0, fmt.Errorf("seal quota override: reply named account %q, not %q", reply.AccountID, accountID)
	}
	if reply.Disposition != admissionv1.AccountQuotaOverrideDispositionSealed {
		return 0, fmt.Errorf("seal quota override: unexpected disposition %q", reply.Disposition)
	}
	if reply.Revision == 0 {
		return 0, errors.New("seal quota override: reply carried no revision")
	}
	return reply.Revision, nil
}

// quotaOverrideSealReaper seals the quota owner's override record in
// StageAccount. List does no I/O and stays pending until this instance has
// sealed, so DryRun never seals; state is per engine, so a fresh run seals again.
type quotaOverrideSealReaper struct {
	sealer     quotaOverrideSealer
	capability sealCapabilityChecker

	mu     sync.Mutex
	sealed map[string]uint64 // account ID -> leader revision, this run only
}

func (r *quotaOverrideSealReaper) Kind() string { return "quota-override" }
func (r *quotaOverrideSealReaper) Stage() Stage { return StageAccount }

func (r *quotaOverrideSealReaper) alreadySealed(accountID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.sealed[accountID]
	return ok
}

func (r *quotaOverrideSealReaper) List(_ context.Context, accountID string) ([]Resource, error) {
	if r.alreadySealed(accountID) {
		return nil, nil
	}
	return []Resource{{Kind: r.Kind(), ID: accountID, Detail: "seal"}}, nil
}

// Delete probes every configured gateway immediately before each seal attempt,
// never reusing an earlier probe. Incomplete capability sends no seal and is
// retryable; force is ignored, since nothing may force past the probe.
func (r *quotaOverrideSealReaper) Delete(ctx context.Context, accountID string, _ Resource, _ bool) error {
	report := r.capability.CheckSealCapability(ctx, accountID)
	if !report.Complete {
		return errQuotaSealCapabilityIncomplete(report)
	}

	revision, err := r.sealer.SealAccountQuotaOverride(ctx, accountID)
	if err != nil {
		return err
	}

	r.mu.Lock()
	if r.sealed == nil {
		r.sealed = map[string]uint64{}
	}
	r.sealed[accountID] = revision
	r.mu.Unlock()
	slog.InfoContext(ctx, "Account teardown sealed quota override", "accountID", accountID, "revision", revision)
	return nil
}

// QuotaOverrideSealReapers returns the StageAccount reaper that seals the
// account's quota override record. gatewayNodeIDs is the configured set of
// nodes that run awsgw, the only processes that write overrides; an empty set
// is unknown, never complete.
func QuotaOverrideSealReapers(nc *nats.Conn, gatewayNodeIDs []string) []Reaper {
	return []Reaper{&quotaOverrideSealReaper{
		sealer:     newQuotaOverrideSealClient(nc),
		capability: newQuotaSealCapabilityClient(nc, gatewayNodeIDs),
	}}
}
