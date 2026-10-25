package accountteardown

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

// ec2AccountSettingsSealTimeout bounds one seal request; an earlier caller
// deadline wins.
const ec2AccountSettingsSealTimeout = 30 * time.Second

// ec2AccountSettingsSealer is EC2's seal of one account's settings record.
type ec2AccountSettingsSealer interface {
	SealAccountSettings(ctx context.Context, accountID string) (revision uint64, err error)
}

// ec2AccountSettingsSealClient reaches the EC2 settings-seal contract over
// NATS. Only a reply for this account, disposition sealed and a non-zero
// revision is a seal; anything else, including no responder, is an error.
type ec2AccountSettingsSealClient struct {
	nc      *nats.Conn
	timeout time.Duration
}

var _ ec2AccountSettingsSealer = (*ec2AccountSettingsSealClient)(nil)

func newEC2AccountSettingsSealClient(nc *nats.Conn) *ec2AccountSettingsSealClient {
	return &ec2AccountSettingsSealClient{nc: nc, timeout: ec2AccountSettingsSealTimeout}
}

func (c *ec2AccountSettingsSealClient) SealAccountSettings(ctx context.Context, accountID string) (uint64, error) {
	timeout := c.timeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline))
	}
	if ctx.Err() != nil || timeout <= 0 {
		return 0, fmt.Errorf("seal account settings: %w", context.DeadlineExceeded)
	}

	reply, err := natsmsg.NATSRequest[ec2v1.SealAccountSettingsReply](ctx, c.nc, ec2v1.SealAccountSettingsSubject,
		&ec2v1.SealAccountSettingsRequest{AccountID: accountID}, timeout, accountID)
	if err != nil {
		return 0, fmt.Errorf("seal account settings: %w", err)
	}
	if reply.AccountID != accountID {
		return 0, fmt.Errorf("seal account settings: reply named account %q, not %q", reply.AccountID, accountID)
	}
	if reply.Disposition != ec2v1.AccountSettingsDispositionSealed {
		return 0, fmt.Errorf("seal account settings: unexpected disposition %q", reply.Disposition)
	}
	if reply.Revision == 0 {
		return 0, errors.New("seal account settings: reply carried no revision")
	}
	return reply.Revision, nil
}

// accountSettingsSealReaper seals EC2's settings record in StageAccount. List
// does no I/O and stays pending until this instance has sealed, so DryRun never
// seals; state is per engine, so every fresh teardown run seals again.
type accountSettingsSealReaper struct {
	sealer     ec2AccountSettingsSealer
	capability sealCapabilityChecker

	mu     sync.Mutex
	sealed map[string]uint64 // account ID -> leader revision, this run only
}

func (r *accountSettingsSealReaper) Kind() string { return "ec2-account-settings" }
func (r *accountSettingsSealReaper) Stage() Stage { return StageAccount }

func (r *accountSettingsSealReaper) alreadySealed(accountID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.sealed[accountID]
	return ok
}

func (r *accountSettingsSealReaper) List(_ context.Context, accountID string) ([]Resource, error) {
	if r.alreadySealed(accountID) {
		return nil, nil
	}
	return []Resource{{Kind: r.Kind(), ID: accountID, Detail: "seal"}}, nil
}

// Delete probes every configured daemon immediately before each seal attempt,
// never reusing an earlier probe. Incomplete capability sends no seal and is
// retryable; force is ignored, since nothing may force past the probe.
func (r *accountSettingsSealReaper) Delete(ctx context.Context, accountID string, _ Resource, _ bool) error {
	report := r.capability.CheckSealCapability(ctx, accountID)
	if !report.Complete {
		return errSealCapabilityIncomplete(report)
	}

	revision, err := r.sealer.SealAccountSettings(ctx, accountID)
	if err != nil {
		return err
	}

	r.mu.Lock()
	if r.sealed == nil {
		r.sealed = map[string]uint64{}
	}
	r.sealed[accountID] = revision
	r.mu.Unlock()
	slog.InfoContext(ctx, "Account teardown sealed EC2 account settings", "accountID", accountID, "revision", revision)
	return nil
}

// EC2AccountSettingsReapers returns the StageAccount reaper that seals EC2's
// settings record. nodeIDs is the configured node set the capability probe
// treats as "every relevant writer"; an empty set is unknown, never complete.
func EC2AccountSettingsReapers(nc *nats.Conn, nodeIDs []string) []Reaper {
	return []Reaper{&accountSettingsSealReaper{
		sealer:     newEC2AccountSettingsSealClient(nc),
		capability: newSealCapabilityClient(nc, nodeIDs),
	}}
}
