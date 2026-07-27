package handlers_eks

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// errBootstrapPermanent marks failures that retrying won't fix (malformed envelope,
// non-base64 CA, JWKS kid mismatch). Transient errors (KV blips) are NOT wrapped.
var errBootstrapPermanent = errors.New("bootstrap permanent error")

const (
	// bootstrapPersistAttempts bounds inline retries on transient errors.
	// Core-NATS delivers each publication once, so KV blips must be retried in-handler.
	bootstrapPersistAttempts = 3
	bootstrapPersistBackoff  = 200 * time.Millisecond
)

// persistWithRetry runs op, retrying on transient errors up to
// bootstrapPersistAttempts. A nil result or an errBootstrapPermanent result
// returns immediately (no retry).
func persistWithRetry(op func() error) error {
	var err error
	for attempt := 1; attempt <= bootstrapPersistAttempts; attempt++ {
		err = op()
		if err == nil || errors.Is(err, errBootstrapPermanent) {
			return err
		}
		if attempt < bootstrapPersistAttempts {
			time.Sleep(bootstrapPersistBackoff)
		}
	}
	return err
}

// Bootstrap subject kinds. Full subject is
// "eks.bus.{accountID}.{clusterName}.{kind}" — see BootstrapSubject.
const (
	BootstrapSubjectToken      = "k3s-bootstrap-token" //nolint:gosec // subject suffix, not a credential
	BootstrapSubjectKubeconfig = "k3s-admin-kubeconfig"
	BootstrapSubjectJWKS       = "k3s-oidc-jwks"
	BootstrapSubjectCA         = "k3s-ca"
)

// BootstrapSubject returns the per-cluster bootstrap subject for one kind.
func BootstrapSubject(accountID, clusterName, kind string) string {
	return fmt.Sprintf("eks.bus.%s.%s.%s", accountID, clusterName, kind)
}

// BootstrapEnvelope is the JSON wire shape for k3s-first-boot.sh publications.
// Each subject populates its matching field; unused fields stay empty.
type BootstrapEnvelope struct {
	// Token is the K3s bootstrap node-token for the cluster (k3s-bootstrap-token).
	Token string `json:"token,omitempty"`
	// AdminKubeconfig is the cluster-admin kubeconfig YAML.
	AdminKubeconfig string `json:"adminKubeconfig,omitempty"`
	// JWKS is the K3s-published JWKS document; cross-checked against the controller-generated keypair.
	JWKS string `json:"jwks,omitempty"`
	// CertificateAuthority is the base64-encoded K3s server CA PEM; written to meta.CertificateAuthorityB64.
	CertificateAuthority string `json:"certificateAuthority,omitempty"`
}

// NATSBootstrap collects the four single-shot bootstrap publications from the K3s VM,
// validates the OIDC JWKS, and persists each artifact to KV. Reconciler observes
// completion via the four KV keys + meta.CertificateAuthorityB64.
type NATSBootstrap struct {
	nc          *nats.Conn
	kv          jetstream.KeyValue
	masterKey   []byte
	accountID   string
	clusterName string
}

// NewNATSBootstrap validates inputs and returns a ready subscriber. Run must
// be called to actually subscribe; constructor is side-effect-free.
func NewNATSBootstrap(nc *nats.Conn, kv jetstream.KeyValue, masterKey []byte, accountID, clusterName string) (*NATSBootstrap, error) {
	if nc == nil {
		return nil, errors.New("eks: NewNATSBootstrap nil nats conn")
	}
	if kv == nil {
		return nil, errors.New("eks: NewNATSBootstrap nil kv")
	}
	if len(masterKey) == 0 {
		return nil, errors.New("eks: NewNATSBootstrap empty master key")
	}
	if accountID == "" {
		return nil, errors.New("eks: NewNATSBootstrap empty accountID")
	}
	if clusterName == "" {
		return nil, errors.New("eks: NewNATSBootstrap empty clusterName")
	}
	return &NATSBootstrap{
		nc:          nc,
		kv:          kv,
		masterKey:   masterKey,
		accountID:   accountID,
		clusterName: clusterName,
	}, nil
}

// BootstrapPendingKinds reports which bootstrap artifacts are still missing so
// a daemon-restart resumes only on pending subjects (K3s publishes each once).
// JWKS is tracked via OIDCJWKSVerifiedKey (set after cross-check), not OIDCJWKSKey
// (pre-seeded by the controller), so ACTIVE always implies a verified keypair.
func BootstrapPendingKinds(ctx context.Context, kv jetstream.KeyValue, clusterName string, meta *ClusterMeta) []string {
	var pending []string
	if _, err := kv.Get(ctx, NodeTokenKey(clusterName)); err != nil {
		pending = append(pending, BootstrapSubjectToken)
	}
	if _, err := kv.Get(ctx, AdminKubeconfigKey(clusterName)); err != nil {
		pending = append(pending, BootstrapSubjectKubeconfig)
	}
	if _, err := kv.Get(ctx, OIDCJWKSVerifiedKey(clusterName)); err != nil {
		pending = append(pending, BootstrapSubjectJWKS)
	}
	if meta == nil || meta.CertificateAuthorityB64 == "" {
		pending = append(pending, BootstrapSubjectCA)
	}
	return pending
}

type bootstrapSubjectHandler struct {
	subject string
	kind    string
	handler func(context.Context, []byte) error
}

// subjectHandlers returns the subject/handler entry for each requested kind.
// An unknown kind is an error — callers pass kinds from BootstrapSubject*
// constants. A nil/empty kinds slice means all four.
func (b *NATSBootstrap) subjectHandlers(kinds []string) ([]bootstrapSubjectHandler, error) {
	all := map[string]func(context.Context, []byte) error{
		BootstrapSubjectToken:      b.persistToken,
		BootstrapSubjectKubeconfig: b.persistKubeconfig,
		BootstrapSubjectJWKS:       b.persistJWKS,
		BootstrapSubjectCA:         b.persistCA,
	}
	if len(kinds) == 0 {
		kinds = []string{BootstrapSubjectToken, BootstrapSubjectKubeconfig, BootstrapSubjectJWKS, BootstrapSubjectCA}
	}
	out := make([]bootstrapSubjectHandler, 0, len(kinds))
	for _, kind := range kinds {
		handler, ok := all[kind]
		if !ok {
			return nil, fmt.Errorf("eks: unknown bootstrap kind %q", kind)
		}
		out = append(out, bootstrapSubjectHandler{
			subject: BootstrapSubject(b.accountID, b.clusterName, kind),
			kind:    kind,
			handler: handler,
		})
	}
	return out, nil
}

// Run subscribes to all four bootstrap subjects and returns when all four have
// arrived, ctx is cancelled, or a handler errors. See RunForKinds.
func (b *NATSBootstrap) Run(ctx context.Context) error {
	return b.RunForKinds(ctx, nil)
}

// RunForKinds subscribes to the requested bootstrap subjects (nil = all four) and
// returns when all arrive, ctx is cancelled, or a handler errors. Drives daemon-restart
// recovery by re-subscribing only to missing artifacts. Each subject is consumed at most once.
func (b *NATSBootstrap) RunForKinds(ctx context.Context, kinds []string) error {
	subs, err := b.subjectHandlers(kinds)
	if err != nil {
		return err
	}
	want := len(subs)

	errCh := make(chan error, want)
	doneCh := make(chan struct{}, 1)
	var (
		mu   sync.Mutex
		done = make(map[string]struct{}, want)
	)

	var subscriptions []*nats.Subscription
	defer func() {
		for _, sub := range subscriptions {
			_ = sub.Unsubscribe()
		}
	}()

	for _, s := range subs {
		sub, err := b.nc.Subscribe(s.subject, func(m *nats.Msg) {
			mu.Lock()
			if _, ok := done[s.kind]; ok {
				mu.Unlock()
				return
			}
			mu.Unlock()
			if err := persistWithRetry(func() error { return s.handler(ctx, m.Data) }); err != nil {
				errCh <- fmt.Errorf("persist %s: %w", s.kind, err)
				return
			}
			mu.Lock()
			done[s.kind] = struct{}{}
			full := len(done) == want
			mu.Unlock()
			if full {
				select {
				case doneCh <- struct{}{}:
				default:
				}
			}
		})
		if err != nil {
			return fmt.Errorf("subscribe %s: %w", s.subject, err)
		}
		subscriptions = append(subscriptions, sub)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			return err
		case <-doneCh:
			slog.Info("NATSBootstrap: all requested bootstrap messages received",
				"accountID", b.accountID, "cluster", b.clusterName, "kinds", want)
			return nil
		}
	}
}

func (b *NATSBootstrap) persistToken(ctx context.Context, data []byte) error {
	env, err := unmarshalBootstrapEnvelope(data)
	if err != nil {
		return err
	}
	if env.Token == "" {
		return fmt.Errorf("%w: token envelope empty", errBootstrapPermanent)
	}
	ct, err := handlers_iam.EncryptSecret(env.Token, b.masterKey)
	if err != nil {
		return fmt.Errorf("encrypt token: %w", err)
	}
	if _, err := b.kv.Put(ctx, NodeTokenKey(b.clusterName), []byte(ct)); err != nil {
		return fmt.Errorf("kv put %s: %w", NodeTokenKey(b.clusterName), err)
	}
	return nil
}

func (b *NATSBootstrap) persistKubeconfig(ctx context.Context, data []byte) error {
	env, err := unmarshalBootstrapEnvelope(data)
	if err != nil {
		return err
	}
	if env.AdminKubeconfig == "" {
		return fmt.Errorf("%w: adminKubeconfig envelope empty", errBootstrapPermanent)
	}
	ct, err := handlers_iam.EncryptSecret(env.AdminKubeconfig, b.masterKey)
	if err != nil {
		return fmt.Errorf("encrypt kubeconfig: %w", err)
	}
	if _, err := b.kv.Put(ctx, AdminKubeconfigKey(b.clusterName), []byte(ct)); err != nil {
		return fmt.Errorf("kv put %s: %w", AdminKubeconfigKey(b.clusterName), err)
	}
	return nil
}

func (b *NATSBootstrap) persistJWKS(ctx context.Context, data []byte) error {
	env, err := unmarshalBootstrapEnvelope(data)
	if err != nil {
		return err
	}
	if env.JWKS == "" {
		return fmt.Errorf("%w: jwks envelope empty", errBootstrapPermanent)
	}
	existing, err := b.kv.Get(ctx, OIDCJWKSKey(b.clusterName))
	if err != nil {
		return fmt.Errorf("kv get %s: %w", OIDCJWKSKey(b.clusterName), err)
	}
	if err := assertJWKSMatch(existing.Value(), []byte(env.JWKS)); err != nil {
		return err
	}
	// Reconciler gates ACTIVE on this verified-marker, not OIDCJWKSKey (pre-seeded by controller).
	if _, err := b.kv.Put(ctx, OIDCJWKSVerifiedKey(b.clusterName), []byte("verified")); err != nil {
		return fmt.Errorf("kv put %s: %w", OIDCJWKSVerifiedKey(b.clusterName), err)
	}
	return nil
}

func (b *NATSBootstrap) persistCA(ctx context.Context, data []byte) error {
	env, err := unmarshalBootstrapEnvelope(data)
	if err != nil {
		return err
	}
	if env.CertificateAuthority == "" {
		return fmt.Errorf("%w: CA envelope empty", errBootstrapPermanent)
	}
	if _, err := base64.StdEncoding.DecodeString(env.CertificateAuthority); err != nil {
		return fmt.Errorf("%w: CA not base64: %w", errBootstrapPermanent, err)
	}
	return SetClusterCertificateAuthority(ctx, b.kv, b.clusterName, env.CertificateAuthority)
}

func unmarshalBootstrapEnvelope(data []byte) (BootstrapEnvelope, error) {
	var env BootstrapEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return env, fmt.Errorf("%w: unmarshal envelope: %w", errBootstrapPermanent, err)
	}
	return env, nil
}

// assertJWKSMatch verifies every incoming JWKS key matches the controller-generated
// keypair by kid and kty; a mismatch rejects the publication.
func assertJWKSMatch(existing, incoming []byte) error {
	var ex, in oidcJWKS
	if err := json.Unmarshal(existing, &ex); err != nil {
		return fmt.Errorf("unmarshal existing JWKS: %w", err)
	}
	if err := json.Unmarshal(incoming, &in); err != nil {
		return fmt.Errorf("%w: unmarshal incoming JWKS: %w", errBootstrapPermanent, err)
	}
	if len(in.Keys) == 0 {
		return fmt.Errorf("%w: incoming JWKS has no keys", errBootstrapPermanent)
	}
	have := make(map[string]oidcJWK, len(ex.Keys))
	for _, k := range ex.Keys {
		have[k.Kid] = k
	}
	for _, k := range in.Keys {
		exKey, ok := have[k.Kid]
		if !ok {
			return fmt.Errorf("%w: bootstrap JWKS kid %q does not match generated keypair", errBootstrapPermanent, k.Kid)
		}
		if k.Kty != exKey.Kty {
			return fmt.Errorf("%w: bootstrap JWKS kid %q key type %q does not match generated %q",
				errBootstrapPermanent, k.Kid, k.Kty, exKey.Kty)
		}
	}
	return nil
}
