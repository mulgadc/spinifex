package handlers_eks

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	defaultReconcileLeaseRefresh = 20 * time.Second
	defaultReconcileInterval     = 30 * time.Second
	defaultHealthzTimeout        = 5 * time.Second
	// defaultCreateTimeout bounds how long a cluster may sit in CREATING before
	// being marked FAILED. Measured from meta.CreatedAt so a VM that never boots
	// reaches a terminal state even across daemon restarts.
	defaultCreateTimeout = 15 * time.Minute
	// defaultStateStaleAfter is the maximum age of a CP state report before
	// health is treated as unknown. CP publishes every ~30s; 3× tolerates a missed tick.
	defaultStateStaleAfter = 90 * time.Second
	// defaultCPRestartGrace is how long an ACTIVE cluster's control plane may stay
	// unhealthy before an in-place restart is attempted. Absorbs transient blips
	// (a missed report, a brief apiserver stall) so a healthy CP is never bounced.
	defaultCPRestartGrace = 2 * time.Minute
	// defaultCPRestartBackoff is the minimum spacing between CP restart attempts.
	defaultCPRestartBackoff = 2 * time.Minute
	// defaultMaxCPRestartAttempts bounds in-place restarts before the reconciler
	// gives up and leaves the cluster degraded for Phase-2 restore / operator action.
	defaultMaxCPRestartAttempts = 3
)

// CPInstanceControl is the minimal control-plane VM lifecycle surface the
// reconciler needs to recover a wedged CP in place. A stopped/error CP is
// restarted onto its existing root volume, so embedded etcd survives. Nil
// disables auto-restart; degradation is still reflected in cluster health.
type CPInstanceControl interface {
	// InstanceState returns the CP instance lifecycle state, e.g. "running",
	// "stopped", "error". Errors propagate so the reconciler retries next tick.
	InstanceState(ctx context.Context, instanceID string) (string, error)
	// StartInstance restarts a stopped/error CP instance in place.
	StartInstance(ctx context.Context, instanceID string) error
}

// natsSubscriber is the minimal subscribe surface the reconciler needs; *nats.Conn satisfies it.
type natsSubscriber interface {
	Subscribe(subject string, cb nats.MsgHandler) (*nats.Subscription, error)
}

// ErrReconcilerLeaseLost is returned by Run when the leader lease is lost mid-loop.
var ErrReconcilerLeaseLost = errors.New("eks: reconciler lease lost")

// ErrReconcilerClusterDeleting is returned by Run when meta.Status is DELETING;
// DeleteCluster owns teardown so the reconciler exits.
var ErrReconcilerClusterDeleting = errors.New("eks: cluster deleting")

// ErrReconcilerClusterFailed is returned by Run when meta.Status is FAILED.
var ErrReconcilerClusterFailed = errors.New("eks: cluster failed")

// HTTPDoer is the minimal http.Client surface needed for /healthz probes.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// ClusterReconciler drives one cluster's CREATING → ACTIVE → DELETING lifecycle
// under a leader lease. One instance per (accountID, clusterName).
type ClusterReconciler struct {
	leaderKV    nats.KeyValue
	acctKV      nats.KeyValue
	accountID   string
	clusterName string
	holderID    string
	healthURL   string

	leaseRefresh  time.Duration
	interval      time.Duration
	healthTimeout time.Duration
	createTimeout time.Duration
	httpClient    HTTPDoer

	// When stateSub is non-nil, health is gated on the CP's NATS self-report
	// rather than the HTTP /healthz probe (apiserver is VPC-only, host-unreachable).
	stateSub        natsSubscriber
	stateSubject    string
	stateStaleAfter time.Duration
	latest          atomic.Pointer[ServerStateReport]

	// Add-on delivery status source. When addonStatusSub is non-nil the
	// reconciler subscribes to the per-cluster add-on status subject and CASes
	// each AddonRecord (CREATING → ACTIVE/DEGRADED) from the on-VM addon-sync
	// agent's reports. Add-on lifecycle is tracked per-resource (AWS parity), so
	// this is a sibling of the cluster state-report, not folded into it.
	addonStatusSub     natsSubscriber
	addonStatusSubject string

	// cpControl recovers a wedged control-plane VM in place; nil disables
	// auto-restart. restart* fields tune the grace window before the first
	// restart, the spacing between attempts, and the attempt cap. degradedSince,
	// restartAttempts, and lastRestartAt are per-cluster restart bookkeeping,
	// mutated only from the single reconcile goroutine.
	cpControl          CPInstanceControl
	restartGrace       time.Duration
	restartBackoff     time.Duration
	maxRestartAttempts int
	degradedSince      time.Time
	restartAttempts    int
	lastRestartAt      time.Time
}

// ReconcilerOption tunes a ClusterReconciler.
type ReconcilerOption func(*ClusterReconciler)

// WithReconcileInterval overrides the default reconcile loop period.
func WithReconcileInterval(d time.Duration) ReconcilerOption {
	return func(r *ClusterReconciler) { r.interval = d }
}

// WithLeaseRefresh overrides the leader-lease refresh period (should be < KVBucketEKSLeaderTTL).
func WithLeaseRefresh(d time.Duration) ReconcilerOption {
	return func(r *ClusterReconciler) { r.leaseRefresh = d }
}

// WithHealthzTimeout overrides the per-probe HTTP timeout.
func WithHealthzTimeout(d time.Duration) ReconcilerOption {
	return func(r *ClusterReconciler) { r.healthTimeout = d }
}

// WithCreateTimeout overrides the CREATING→FAILED timeout.
func WithCreateTimeout(d time.Duration) ReconcilerOption {
	return func(r *ClusterReconciler) { r.createTimeout = d }
}

// WithHTTPClient injects a stub HTTPDoer (tests).
func WithHTTPClient(c HTTPDoer) ReconcilerOption {
	return func(r *ClusterReconciler) { r.httpClient = c }
}

// WithStateSource gates health on the CP's NATS state report instead of an
// HTTP /healthz probe. The subscription is opened in Run and closed on return.
func WithStateSource(sub natsSubscriber, subject string) ReconcilerOption {
	return func(r *ClusterReconciler) {
		r.stateSub = sub
		r.stateSubject = subject
	}
}

// WithStateStaleAfter overrides the maximum age of a state report before health is treated as failing.
func WithStateStaleAfter(d time.Duration) ReconcilerOption {
	return func(r *ClusterReconciler) { r.stateStaleAfter = d }
}

// WithAddonStatusSource configures the reconciler to consume per-add-on delivery
// status reports (subject) via sub and CAS the matching AddonRecord. The
// subscription is opened in Run and closed when it returns.
func WithAddonStatusSource(sub natsSubscriber, subject string) ReconcilerOption {
	return func(r *ClusterReconciler) {
		r.addonStatusSub = sub
		r.addonStatusSubject = subject
	}
}

// WithCPInstanceControl injects the control-plane VM lifecycle surface used to
// restart a wedged CP in place. Absent, the reconciler only reflects health.
func WithCPInstanceControl(c CPInstanceControl) ReconcilerOption {
	return func(r *ClusterReconciler) { r.cpControl = c }
}

// WithCPRestartPolicy overrides the CP auto-restart grace window, attempt
// spacing, and max attempts. Non-positive values keep the default.
func WithCPRestartPolicy(grace, backoff time.Duration, maxAttempts int) ReconcilerOption {
	return func(r *ClusterReconciler) {
		if grace > 0 {
			r.restartGrace = grace
		}
		if backoff > 0 {
			r.restartBackoff = backoff
		}
		if maxAttempts > 0 {
			r.maxRestartAttempts = maxAttempts
		}
	}
}

// NewClusterReconciler constructs a ClusterReconciler. healthURL is the
// fully-qualified probe target (e.g. https://nlb-dns:443/healthz) or empty
// to skip /healthz probing (CREATING transitions on bootstrap state alone).
func NewClusterReconciler(leaderKV, acctKV nats.KeyValue, accountID, clusterName, holderID, healthURL string, opts ...ReconcilerOption) (*ClusterReconciler, error) {
	if leaderKV == nil {
		return nil, errors.New("eks: NewClusterReconciler nil leaderKV")
	}
	if acctKV == nil {
		return nil, errors.New("eks: NewClusterReconciler nil acctKV")
	}
	if accountID == "" {
		return nil, errors.New("eks: NewClusterReconciler empty accountID")
	}
	if clusterName == "" {
		return nil, errors.New("eks: NewClusterReconciler empty clusterName")
	}
	if holderID == "" {
		return nil, errors.New("eks: NewClusterReconciler empty holderID")
	}
	r := &ClusterReconciler{
		leaderKV:           leaderKV,
		acctKV:             acctKV,
		accountID:          accountID,
		clusterName:        clusterName,
		holderID:           holderID,
		healthURL:          healthURL,
		leaseRefresh:       defaultReconcileLeaseRefresh,
		interval:           defaultReconcileInterval,
		healthTimeout:      defaultHealthzTimeout,
		createTimeout:      defaultCreateTimeout,
		stateStaleAfter:    defaultStateStaleAfter,
		restartGrace:       defaultCPRestartGrace,
		restartBackoff:     defaultCPRestartBackoff,
		maxRestartAttempts: defaultMaxCPRestartAttempts,
		httpClient: &http.Client{
			Timeout: defaultHealthzTimeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, //nolint:gosec // K3s NLB is self-signed; CA pinning is a v2 hardening item
				},
			},
		},
	}
	for _, o := range opts {
		o(r)
	}
	return r, nil
}

func reconcilerLeaderKey(accountID, clusterName string) string {
	return accountID + "/" + clusterName
}

// AcquireLease attempts a CAS Create on the leader bucket. Returns
// (release, true) on success; (nil, false) otherwise. The bucket TTL reaps
// stale leases when release does not run.
func (r *ClusterReconciler) AcquireLease() (func(), bool) {
	key := reconcilerLeaderKey(r.accountID, r.clusterName)
	if _, err := r.leaderKV.Create(key, []byte(r.holderID)); err != nil {
		slog.Debug("ClusterReconciler: lease held by another holder",
			"key", key, "holderID", r.holderID, "err", err)
		return nil, false
	}
	slog.Info("ClusterReconciler: lease acquired", "key", key, "holderID", r.holderID)
	return func() {
		if err := r.leaderKV.Delete(key); err != nil {
			slog.Warn("ClusterReconciler: lease release failed (TTL will reap)",
				"key", key, "holderID", r.holderID, "err", err)
		}
	}, true
}

// RefreshLease re-asserts ownership via a CAS update. Returns false if another holder won.
func (r *ClusterReconciler) RefreshLease() bool {
	key := reconcilerLeaderKey(r.accountID, r.clusterName)
	entry, err := r.leaderKV.Get(key)
	if err != nil {
		slog.Warn("ClusterReconciler: lease get failed", "key", key, "err", err)
		return false
	}
	if string(entry.Value()) != r.holderID {
		slog.Info("ClusterReconciler: lease now held by another holder",
			"key", key, "holderID", r.holderID, "got", string(entry.Value()))
		return false
	}
	if _, err := r.leaderKV.Update(key, []byte(r.holderID), entry.Revision()); err != nil {
		slog.Warn("ClusterReconciler: lease CAS update failed",
			"key", key, "holderID", r.holderID, "err", err)
		return false
	}
	return true
}

// Run drives reconcile until ctx is cancelled, the lease is lost, or the cluster
// reaches a terminal state. Caller must AcquireLease first.
func (r *ClusterReconciler) Run(ctx context.Context) error {
	if r.stateSub != nil && r.stateSubject != "" {
		sub, err := r.stateSub.Subscribe(r.stateSubject, func(m *nats.Msg) {
			report, perr := unmarshalServerStateReport(m.Data)
			if perr != nil {
				slog.Warn("ClusterReconciler: bad state report",
					"cluster", r.clusterName, "subject", m.Subject, "err", perr)
				return
			}
			r.latest.Store(&report)
		})
		if err != nil {
			return fmt.Errorf("subscribe state report %s: %w", r.stateSubject, err)
		}
		defer func() { _ = sub.Unsubscribe() }()
	}

	if r.addonStatusSub != nil && r.addonStatusSubject != "" {
		sub, err := r.addonStatusSub.Subscribe(r.addonStatusSubject, func(m *nats.Msg) {
			report, perr := unmarshalAddonStatusReport(m.Data)
			if perr != nil {
				slog.Warn("ClusterReconciler: bad addon status report",
					"cluster", r.clusterName, "subject", m.Subject, "err", perr)
				return
			}
			r.applyAddonStatusReport(report)
		})
		if err != nil {
			return fmt.Errorf("subscribe addon status %s: %w", r.addonStatusSubject, err)
		}
		defer func() { _ = sub.Unsubscribe() }()
	}

	refreshT := time.NewTicker(r.leaseRefresh)
	defer refreshT.Stop()
	reconcileT := time.NewTicker(r.interval)
	defer reconcileT.Stop()

	if err := r.reconcileOnce(ctx); err != nil {
		if terminalReconcileErr(err) {
			return err
		}
		slog.Warn("ClusterReconciler: initial reconcile failed",
			"cluster", r.clusterName, "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-refreshT.C:
			if !r.RefreshLease() {
				return ErrReconcilerLeaseLost
			}
		case <-reconcileT.C:
			if err := r.reconcileOnce(ctx); err != nil {
				if terminalReconcileErr(err) {
					return err
				}
				slog.Warn("ClusterReconciler: reconcile failed",
					"cluster", r.clusterName, "err", err)
			}
		}
	}
}

func terminalReconcileErr(err error) bool {
	return errors.Is(err, ErrReconcilerClusterDeleting) ||
		errors.Is(err, ErrReconcilerClusterFailed) ||
		errors.Is(err, ErrClusterNotFound)
}

func (r *ClusterReconciler) reconcileOnce(ctx context.Context) error {
	meta, err := GetClusterMeta(r.acctKV, r.clusterName)
	if err != nil {
		return err
	}
	switch meta.Status {
	case ClusterStatusCreating:
		ready, reason := r.bootstrapReady(meta)
		if !ready {
			// Info, not Debug: this is the per-tick reason a CREATING cluster has
			// not yet flipped ACTIVE. At Debug it is invisible at the default
			// level, so a cluster that stalls to the create-timeout FAILED gives
			// no diagnosable cause. Logged only during the CREATING window.
			slog.Info("ClusterReconciler: bootstrap not ready",
				"cluster", r.clusterName, "reason", reason)
			return r.failIfCreateTimedOut(meta, "bootstrap not ready: "+reason)
		}
		issue, nodeCount := r.observe(ctx)
		if issue != "" {
			slog.Info("ClusterReconciler: health not ready",
				"cluster", r.clusterName, "issue", issue)
			return r.failIfCreateTimedOut(meta, "healthz not ready: "+issue)
		}
		if err := SetClusterStatus(r.acctKV, r.clusterName, ClusterStatusActive); err != nil {
			return err
		}
		if err := SetClusterHealthState(r.acctKV, r.clusterName, "", nodeCount); err != nil {
			if errors.Is(err, ErrClusterNotFound) {
				return ErrClusterNotFound
			}
			return fmt.Errorf("record cluster health: %w", err)
		}
		slog.Info("ClusterReconciler: cluster transitioned to ACTIVE",
			"cluster", r.clusterName, "nodes", nodeCount)
	case ClusterStatusActive:
		issue, nodeCount := r.observe(ctx)
		if issue != "" {
			slog.Warn("ClusterReconciler: health failing for ACTIVE cluster",
				"cluster", r.clusterName, "issue", issue)
		}
		if err := SetClusterHealthState(r.acctKV, r.clusterName, issue, nodeCount); err != nil {
			if errors.Is(err, ErrClusterNotFound) {
				return ErrClusterNotFound
			}
			return fmt.Errorf("record cluster health: %w", err)
		}
		r.maybeRecoverControlPlane(ctx, meta, issue)
	case ClusterStatusDeleting:
		return ErrReconcilerClusterDeleting
	case ClusterStatusFailed:
		return ErrReconcilerClusterFailed
	}
	return nil
}

// maybeRecoverControlPlane attempts a bounded in-place restart of a wedged
// control-plane VM. A healthy CP clears the degraded clock and attempt count. An
// unhealthy CP is left alone until it stays unhealthy past restartGrace, then
// StartInstance is invoked (once per restartBackoff, up to maxRestartAttempts)
// only when the instance is stopped/error — a restart re-mounts the same root
// volume so embedded etcd survives. Best-effort: failures log and retry next
// tick; the cluster stays degraded for operator/Phase-2 DR.
func (r *ClusterReconciler) maybeRecoverControlPlane(ctx context.Context, meta *ClusterMeta, issue string) {
	if issue == "" {
		r.degradedSince = time.Time{}
		r.restartAttempts = 0
		return
	}
	members := cpMemberInstanceIDs(meta)
	if r.cpControl == nil || len(members) == 0 {
		return
	}
	now := time.Now()
	if r.degradedSince.IsZero() {
		r.degradedSince = now
	}
	if now.Sub(r.degradedSince) < r.restartGrace {
		return
	}
	if r.restartAttempts >= r.maxRestartAttempts {
		return
	}
	if !r.lastRestartAt.IsZero() && now.Sub(r.lastRestartAt) < r.restartBackoff {
		return
	}

	// Collect every CP member currently restartable (stopped/error). An HA
	// control plane needs etcd quorum, so recovering only the primary can leave
	// the apiserver down (quorum 1/3); restart all wedged members in one pass. A
	// per-member state query failure logs and skips that member, not the rest.
	var restartable []string
	for _, id := range members {
		state, err := r.cpControl.InstanceState(ctx, id)
		if err != nil {
			slog.Warn("ClusterReconciler: CP instance state query failed",
				"cluster", r.clusterName, "instanceId", id, "err", err)
			continue
		}
		if cpRestartable(state) {
			restartable = append(restartable, id)
		}
	}
	if len(restartable) == 0 {
		// Members are running (VM-level) but the cluster is still unhealthy — a
		// restart won't fix wedged etcd; leave degraded for Phase-2 snapshot restore.
		return
	}

	r.lastRestartAt = now
	r.restartAttempts++
	for _, id := range restartable {
		slog.Warn("ClusterReconciler: restarting wedged control-plane member in place",
			"cluster", r.clusterName, "instanceId", id,
			"attempt", r.restartAttempts, "members", len(members), "issue", issue)
		if err := r.cpControl.StartInstance(ctx, id); err != nil {
			slog.Warn("ClusterReconciler: CP restart failed",
				"cluster", r.clusterName, "instanceId", id,
				"attempt", r.restartAttempts, "err", err)
			continue
		}
		slog.Info("ClusterReconciler: control-plane restart issued",
			"cluster", r.clusterName, "instanceId", id, "attempt", r.restartAttempts)
	}
}

// cpMemberInstanceIDs returns every control-plane member instance ID from the
// cluster meta. HA clusters list all servers in ControlPlaneNodes; older or
// single-CP clusters only carry the scalar ControlPlaneInstanceID.
func cpMemberInstanceIDs(meta *ClusterMeta) []string {
	if len(meta.ControlPlaneNodes) > 0 {
		ids := make([]string, 0, len(meta.ControlPlaneNodes))
		for _, n := range meta.ControlPlaneNodes {
			if n.InstanceID != "" {
				ids = append(ids, n.InstanceID)
			}
		}
		if len(ids) > 0 {
			return ids
		}
	}
	if meta.ControlPlaneInstanceID != "" {
		return []string{meta.ControlPlaneInstanceID}
	}
	return nil
}

// cpRestartable reports whether an in-place StartInstance can plausibly recover a
// CP in the given state. Only a stopped/error instance (VM died, but the
// instance record + root volume survive) is restartable; running/pending are not.
func cpRestartable(state string) bool {
	switch state {
	case "stopped", "error":
		return true
	default:
		return false
	}
}

// observe returns the health issue ("" = healthy) and node count from the CP's
// latest NATS self-report, or from the HTTP /healthz probe when no NATS source is set.
func (r *ClusterReconciler) observe(ctx context.Context) (issue string, nodeCount int) {
	if r.stateSub != nil && r.stateSubject != "" {
		report := r.latest.Load()
		if report == nil {
			return "no control-plane state report received yet", 0
		}
		age := time.Since(time.Unix(report.TS, 0))
		if age > r.stateStaleAfter {
			return fmt.Sprintf("control-plane state report stale (%s old)", age.Round(time.Second)), report.NodeCount
		}
		if !report.Healthy() {
			return fmt.Sprintf("apiserver healthz=%q", report.Healthz), report.NodeCount
		}
		return "", report.NodeCount
	}
	if err := r.probeHealthz(ctx); err != nil {
		return err.Error(), 0
	}
	return "", 0
}

// failIfCreateTimedOut marks the cluster FAILED once createTimeout has elapsed
// since meta.CreatedAt. Returns nil while still within the window so the next
// tick retries. A zero CreatedAt disables the deadline.
func (r *ClusterReconciler) failIfCreateTimedOut(meta *ClusterMeta, reason string) error {
	if r.createTimeout <= 0 || meta.CreatedAt.IsZero() {
		return nil
	}
	if time.Since(meta.CreatedAt) < r.createTimeout {
		return nil
	}
	failReason := fmt.Sprintf("cluster did not become ACTIVE within %s: %s", r.createTimeout, reason)
	if err := MarkClusterFailed(r.acctKV, r.clusterName, failReason); err != nil {
		if errors.Is(err, ErrClusterNotFound) {
			return ErrClusterNotFound
		}
		return fmt.Errorf("mark create-timeout failed: %w", err)
	}
	slog.Warn("ClusterReconciler: CREATING timed out, marked FAILED",
		"cluster", r.clusterName, "timeout", r.createTimeout, "reason", reason)
	return ErrReconcilerClusterFailed
}

// bootstrapReady returns true once the four bootstrap KV artifacts are present:
// node token, admin kubeconfig, OIDC JWKS verified-marker, and CA on meta.
// Uses the verified-marker (not OIDCJWKSKey) so ACTIVE always implies a verified key.
func (r *ClusterReconciler) bootstrapReady(meta *ClusterMeta) (bool, string) {
	if meta.CertificateAuthorityB64 == "" {
		return false, "CA absent on meta"
	}
	keys := []string{
		NodeTokenKey(r.clusterName),
		AdminKubeconfigKey(r.clusterName),
		OIDCJWKSVerifiedKey(r.clusterName),
	}
	for _, k := range keys {
		if _, err := r.acctKV.Get(k); err != nil {
			return false, fmt.Sprintf("%s missing: %s", k, err)
		}
	}
	return true, ""
}

func (r *ClusterReconciler) probeHealthz(ctx context.Context) error {
	if r.healthURL == "" {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, r.healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, r.healthURL, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("healthz request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("eks: /healthz status %d", resp.StatusCode)
	}
	return nil
}
