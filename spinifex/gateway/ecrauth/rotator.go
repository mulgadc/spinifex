package gateway_ecrauth

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Rotation defaults (ecr-v1 Q3): rotate the active signing key every 30 days,
// keep the previous 2 keys for at least 24h (well past the 12h token TTL) so
// in-flight tokens stay verifiable, then prune.
const (
	DefaultRotateAfter           = 30 * 24 * time.Hour
	DefaultRetainFor             = 24 * time.Hour
	DefaultRetainCount           = 2
	DefaultRotationCheckInterval = time.Hour

	// rotationCycleTimeout bounds the KV work of one rotation cycle, so a wedged
	// JetStream stalls a single tick instead of the whole loop until shutdown.
	rotationCycleTimeout = 30 * time.Second
)

// rotationParams holds the rotation/retention policy.
type rotationParams struct {
	rotateAfter time.Duration
	retainFor   time.Duration
	retainCount int
}

// planRotation decides, from the current key metadata, whether to mint a new
// active key and which rotated-out keys to prune. It is pure so the policy is
// exhaustively unit-testable; rotateOnce wires KV I/O around it.
//
// The active key is the newest. A mint is due once the active key is older than
// rotateAfter. A previous key is pruned once the active key has stood for
// retainFor (its tokens have expired) or the key ranks beyond retainCount among
// the previous keys. The active key is never pruned: when a mint is due it
// becomes the previous key next cycle and is retained until the fresh key ages
// past retainFor.
func planRotation(metas []keyMeta, now time.Time, p rotationParams) (mint bool, prune []string) {
	if len(metas) == 0 {
		return true, nil
	}
	sorted := append([]keyMeta(nil), metas...)
	sort.Slice(sorted, func(i, j int) bool { return newerKey(sorted[i], sorted[j]) })

	active := sorted[0]
	activeAge := now.Sub(active.created)
	mint = activeAge >= p.rotateAfter

	for rank, m := range sorted[1:] {
		if activeAge >= p.retainFor || rank >= p.retainCount {
			prune = append(prune, m.kid)
		}
	}
	return mint, prune
}

// Rotator periodically rotates and prunes the ECR signing keys, pushing the new
// active key + verification set into the live Issuer and Verifier. It mirrors
// the LifecycleSweeper pattern: a Run loop bound to the process lifetime. It is
// safe to run on every awsgw instance — mint and delete on the shared KV bucket
// converge, and a delete of an already-gone key is a no-op.
type Rotator struct {
	kv        jetstream.KeyValue
	masterKey []byte
	issuer    *Issuer
	verifier  *Verifier
	params    rotationParams
	interval  time.Duration
	now       func() time.Time
}

// NewRotator opens the signing-key bucket and builds a rotator that keeps issuer
// and verifier current. replicas matches the cluster size for first-run bucket
// creation.
func NewRotator(ctx context.Context, js jetstream.JetStream, masterKey []byte, replicas int, issuer *Issuer, verifier *Verifier) (*Rotator, error) {
	kv, err := openSigningBucket(ctx, js, masterKey, replicas)
	if err != nil {
		return nil, err
	}
	return &Rotator{
		kv:        kv,
		masterKey: masterKey,
		issuer:    issuer,
		verifier:  verifier,
		params: rotationParams{
			rotateAfter: DefaultRotateAfter,
			retainFor:   DefaultRetainFor,
			retainCount: DefaultRetainCount,
		},
		interval: DefaultRotationCheckInterval,
		now:      func() time.Time { return time.Now().UTC() },
	}, nil
}

// Run rotates on every tick until ctx is cancelled. Blocks until then.
func (r *Rotator) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	slog.Info("ECR signing-key rotator started", "interval", r.interval, "rotateAfter", r.params.rotateAfter)
	for {
		select {
		case <-ctx.Done():
			slog.Info("ECR signing-key rotator stopped")
			return
		case <-ticker.C:
			cycleCtx, cancel := context.WithTimeout(ctx, rotationCycleTimeout)
			r.rotateOnce(cycleCtx, r.now())
			cancel()
		}
	}
}

// rotateOnce runs one rotation/prune cycle and refreshes the issuer/verifier.
// Errors are logged and skipped so a transient KV fault never crashes the loop.
func (r *Rotator) rotateOnce(ctx context.Context, now time.Time) {
	_, _, metas, err := reloadKeys(ctx, r.kv, r.masterKey)
	if err != nil {
		slog.Warn("ECR signing-key rotation: load keys failed", "err", err)
		return
	}

	mint, prune := planRotation(metas, now, r.params)
	if mint {
		newKey, err := generateSigningKey(ctx, r.kv, r.masterKey)
		if err != nil {
			slog.Warn("ECR signing-key rotation: mint failed", "err", err)
			return
		}
		slog.Info("ECR signing-key rotated", "kid", newKey.Kid)
	}
	for _, kid := range prune {
		if err := deleteSigningKey(ctx, r.kv, kid); err != nil {
			slog.Warn("ECR signing-key rotation: prune failed", "kid", kid, "err", err)
			continue
		}
		slog.Info("ECR signing-key pruned", "kid", kid)
	}

	r.refresh(ctx)
}

// refresh reloads the canonical key set and installs it on the issuer/verifier.
func (r *Rotator) refresh(ctx context.Context) {
	active, verify, _, err := reloadKeys(ctx, r.kv, r.masterKey)
	if err != nil {
		slog.Warn("ECR signing-key rotation: refresh failed", "err", err)
		return
	}
	if active == nil {
		return
	}
	r.issuer.SetActiveKey(active)
	r.verifier.SetKeys(verify)
}
