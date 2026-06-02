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
)

// errBootstrapPermanent marks a bootstrap persist failure that no amount of
// retrying will fix — a malformed envelope, a non-base64 CA, or a JWKS whose
// kid does not match the controller-generated keypair. Transient failures
// (KV write blips, JWKS-not-yet-propagated) are NOT wrapped with this, so
// persistWithRetry keeps retrying them. A permanent error fails the cluster.
var errBootstrapPermanent = errors.New("bootstrap permanent error")

const (
	// bootstrapPersistAttempts bounds inline retries of a single subject's
	// persist op on transient errors. A core-NATS publication is delivered
	// once, so a transient KV blip must be ridden out in-handler rather than
	// by tearing down the subscription and waiting for a re-publish that
	// never comes.
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

	bootstrapSubjectsTotal = 4
)

// BootstrapSubject returns the per-cluster bootstrap subject for one kind.
func BootstrapSubject(accountID, clusterName, kind string) string {
	return fmt.Sprintf("eks.bus.%s.%s.%s", accountID, clusterName, kind)
}

// BootstrapEnvelope is the JSON wire shape every k3s-first-boot.sh
// publication uses. Each subject populates the field matching its kind; the
// rest stay empty. Versioned implicitly by field shape — adding new optional
// fields is backwards-compatible.
type BootstrapEnvelope struct {
	// Token is the K3s bootstrap node-token for the cluster (k3s-bootstrap-token).
	Token string `json:"token,omitempty"`
	// AdminKubeconfig is the cluster-admin kubeconfig YAML
	// (k3s-admin-kubeconfig).
	AdminKubeconfig string `json:"adminKubeconfig,omitempty"`
	// JWKS is the K3s-published RFC 7517 JWKS document. The bootstrap
	// subscriber cross-checks it against the controller-generated keypair
	// (k3s-oidc-jwks).
	JWKS string `json:"jwks,omitempty"`
	// CertificateAuthority is the base64-encoded K3s server CA PEM
	// (k3s-ca). Written onto meta.CertificateAuthorityB64.
	CertificateAuthority string `json:"certificateAuthority,omitempty"`
}

// NATSBootstrap collects the four single-shot bootstrap publications from a
// K3s server VM, validates the OIDC JWKS against what the controller wrote
// pre-VM, persists each artifact into the per-cluster KV bucket, and writes
// the CA onto meta.CertificateAuthorityB64. The reconciler observes
// completion via the four KV keys + meta.CertificateAuthorityB64.
type NATSBootstrap struct {
	nc          *nats.Conn
	kv          nats.KeyValue
	masterKey   []byte
	accountID   string
	clusterName string
}

// NewNATSBootstrap validates inputs and returns a ready subscriber. Run must
// be called to actually subscribe; constructor is side-effect-free.
func NewNATSBootstrap(nc *nats.Conn, kv nats.KeyValue, masterKey []byte, accountID, clusterName string) (*NATSBootstrap, error) {
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

// Run subscribes to the four bootstrap subjects and returns when:
//   - all four have arrived and persisted successfully (nil),
//   - ctx is cancelled (ctx.Err()),
//   - one of the persistence handlers returns an error (first error).
//
// Each subject is consumed at most once; subsequent publications on the same
// subject are dropped. Subscriptions are always cleaned up before return.
func (b *NATSBootstrap) Run(ctx context.Context) error {
	type subjectHandler struct {
		subject string
		kind    string
		handler func([]byte) error
	}
	subs := []subjectHandler{
		{BootstrapSubject(b.accountID, b.clusterName, BootstrapSubjectToken), BootstrapSubjectToken, b.persistToken},
		{BootstrapSubject(b.accountID, b.clusterName, BootstrapSubjectKubeconfig), BootstrapSubjectKubeconfig, b.persistKubeconfig},
		{BootstrapSubject(b.accountID, b.clusterName, BootstrapSubjectJWKS), BootstrapSubjectJWKS, b.persistJWKS},
		{BootstrapSubject(b.accountID, b.clusterName, BootstrapSubjectCA), BootstrapSubjectCA, b.persistCA},
	}

	errCh := make(chan error, bootstrapSubjectsTotal)
	doneCh := make(chan struct{}, 1)
	var (
		mu   sync.Mutex
		done = make(map[string]struct{}, bootstrapSubjectsTotal)
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
			if err := persistWithRetry(func() error { return s.handler(m.Data) }); err != nil {
				errCh <- fmt.Errorf("persist %s: %w", s.kind, err)
				return
			}
			mu.Lock()
			done[s.kind] = struct{}{}
			full := len(done) == bootstrapSubjectsTotal
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
			slog.Info("NATSBootstrap: all four bootstrap messages received",
				"accountID", b.accountID, "cluster", b.clusterName)
			return nil
		}
	}
}

func (b *NATSBootstrap) persistToken(data []byte) error {
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
	if _, err := b.kv.Put(NodeTokenKey(b.clusterName), []byte(ct)); err != nil {
		return fmt.Errorf("kv put %s: %w", NodeTokenKey(b.clusterName), err)
	}
	return nil
}

func (b *NATSBootstrap) persistKubeconfig(data []byte) error {
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
	if _, err := b.kv.Put(AdminKubeconfigKey(b.clusterName), []byte(ct)); err != nil {
		return fmt.Errorf("kv put %s: %w", AdminKubeconfigKey(b.clusterName), err)
	}
	return nil
}

func (b *NATSBootstrap) persistJWKS(data []byte) error {
	env, err := unmarshalBootstrapEnvelope(data)
	if err != nil {
		return err
	}
	if env.JWKS == "" {
		return fmt.Errorf("%w: jwks envelope empty", errBootstrapPermanent)
	}
	existing, err := b.kv.Get(OIDCJWKSKey(b.clusterName))
	if err != nil {
		return fmt.Errorf("kv get %s: %w", OIDCJWKSKey(b.clusterName), err)
	}
	if err := assertJWKSMatch(existing.Value(), []byte(env.JWKS)); err != nil {
		return err
	}
	return nil
}

func (b *NATSBootstrap) persistCA(data []byte) error {
	env, err := unmarshalBootstrapEnvelope(data)
	if err != nil {
		return err
	}
	if env.CertificateAuthority == "" {
		return fmt.Errorf("%w: CA envelope empty", errBootstrapPermanent)
	}
	if _, err := base64.StdEncoding.DecodeString(env.CertificateAuthority); err != nil {
		return fmt.Errorf("%w: CA not base64: %v", errBootstrapPermanent, err)
	}
	return SetClusterCertificateAuthority(b.kv, b.clusterName, env.CertificateAuthority)
}

func unmarshalBootstrapEnvelope(data []byte) (BootstrapEnvelope, error) {
	var env BootstrapEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return env, fmt.Errorf("%w: unmarshal envelope: %v", errBootstrapPermanent, err)
	}
	return env, nil
}

// assertJWKSMatch verifies that every key id (kid) declared in the incoming
// JWKS exists in the controller-generated existing JWKS. Mismatch means the
// VM either failed to consume our PEM or generated its own signing key —
// reject the publication.
func assertJWKSMatch(existing, incoming []byte) error {
	var ex, in oidcJWKS
	if err := json.Unmarshal(existing, &ex); err != nil {
		return fmt.Errorf("unmarshal existing JWKS: %w", err)
	}
	if err := json.Unmarshal(incoming, &in); err != nil {
		return fmt.Errorf("%w: unmarshal incoming JWKS: %v", errBootstrapPermanent, err)
	}
	if len(in.Keys) == 0 {
		return fmt.Errorf("%w: incoming JWKS has no keys", errBootstrapPermanent)
	}
	have := map[string]struct{}{}
	for _, k := range ex.Keys {
		have[k.Kid] = struct{}{}
	}
	for _, k := range in.Keys {
		if _, ok := have[k.Kid]; !ok {
			return fmt.Errorf("%w: bootstrap JWKS kid %q does not match generated keypair", errBootstrapPermanent, k.Kid)
		}
	}
	return nil
}
