package handlers_eks

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	bootstrapTestCluster = "alpha"
)

var bootstrapTestMasterKey = []byte("0123456789abcdef0123456789abcdef")

// natsBootstrapHarness wires an embedded JetStream NATS server, a per-account
// KV bucket, a published controller JWKS (so persistJWKS has something to
// cross-check against), and a NATSBootstrap subscriber.
type natsBootstrapHarness struct {
	t  *testing.T
	nc *nats.Conn
	kv nats.KeyValue

	subscriber *NATSBootstrap
}

func newBootstrapHarness(t *testing.T) *natsBootstrapHarness {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	kv, err := GetOrCreateAccountBucket(js, testAccountID)
	require.NoError(t, err)

	require.NoError(t, PutClusterMeta(kv, sampleClusterMeta(bootstrapTestCluster)))

	// Lay down the controller-generated JWKS so persistJWKS has something
	// to cross-check against.
	_, _, err = GenerateClusterOIDCKeypair(kv, bootstrapTestCluster, bootstrapTestMasterKey)
	require.NoError(t, err)

	sub, err := NewNATSBootstrap(nc, kv, bootstrapTestMasterKey, testAccountID, bootstrapTestCluster)
	require.NoError(t, err)

	return &natsBootstrapHarness{t: t, nc: nc, kv: kv, subscriber: sub}
}

func (h *natsBootstrapHarness) publish(kind string, env BootstrapEnvelope) {
	h.t.Helper()
	data, err := json.Marshal(env)
	require.NoError(h.t, err)
	subject := BootstrapSubject(testAccountID, bootstrapTestCluster, kind)
	require.NoError(h.t, h.nc.Publish(subject, data))
	require.NoError(h.t, h.nc.Flush())
}

func (h *natsBootstrapHarness) loadControllerJWKS() []byte {
	h.t.Helper()
	entry, err := h.kv.Get(OIDCJWKSKey(bootstrapTestCluster))
	require.NoError(h.t, err)
	return entry.Value()
}

func TestNewNATSBootstrap_RejectsBadInputs(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	kv, err := GetOrCreateAccountBucket(js, testAccountID)
	require.NoError(t, err)

	_, err = NewNATSBootstrap(nil, kv, bootstrapTestMasterKey, testAccountID, "alpha")
	require.Error(t, err)
	_, err = NewNATSBootstrap(nc, nil, bootstrapTestMasterKey, testAccountID, "alpha")
	require.Error(t, err)
	_, err = NewNATSBootstrap(nc, kv, nil, testAccountID, "alpha")
	require.Error(t, err)
	_, err = NewNATSBootstrap(nc, kv, bootstrapTestMasterKey, "", "alpha")
	require.Error(t, err)
	_, err = NewNATSBootstrap(nc, kv, bootstrapTestMasterKey, testAccountID, "")
	require.Error(t, err)
}

func TestNATSBootstrap_HappyPath(t *testing.T) {
	h := newBootstrapHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	jwks := string(h.loadControllerJWKS())
	caB64 := base64.StdEncoding.EncodeToString([]byte("-----BEGIN CERTIFICATE-----\nfakeCA\n-----END CERTIFICATE-----\n"))

	runErr := make(chan error, 1)
	go func() { runErr <- h.subscriber.Run(ctx) }()

	// Give subscriptions time to bind before publishing.
	require.Eventually(t, func() bool {
		// Probe by sending a no-op flush — once Run is past Subscribe the
		// subjects are live; we just need a small yield to the goroutine.
		return true
	}, 100*time.Millisecond, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	h.publish(BootstrapSubjectToken, BootstrapEnvelope{Token: "k3s::server-node-token::v1"})
	h.publish(BootstrapSubjectKubeconfig, BootstrapEnvelope{AdminKubeconfig: "apiVersion: v1\nkind: Config\n"})
	h.publish(BootstrapSubjectJWKS, BootstrapEnvelope{JWKS: jwks})
	h.publish(BootstrapSubjectCA, BootstrapEnvelope{CertificateAuthority: caB64})

	select {
	case err := <-runErr:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after all four publications")
	}

	tokEntry, err := h.kv.Get(NodeTokenKey(bootstrapTestCluster))
	require.NoError(t, err)
	dec, err := handlers_iam.DecryptSecret(string(tokEntry.Value()), bootstrapTestMasterKey)
	require.NoError(t, err)
	assert.Equal(t, "k3s::server-node-token::v1", dec)

	kcEntry, err := h.kv.Get(AdminKubeconfigKey(bootstrapTestCluster))
	require.NoError(t, err)
	dec, err = handlers_iam.DecryptSecret(string(kcEntry.Value()), bootstrapTestMasterKey)
	require.NoError(t, err)
	assert.Equal(t, "apiVersion: v1\nkind: Config\n", dec)

	meta, err := GetClusterMeta(h.kv, bootstrapTestCluster)
	require.NoError(t, err)
	assert.Equal(t, caB64, meta.CertificateAuthorityB64)
}

func TestNATSBootstrap_ContextCancelReturnsErr(t *testing.T) {
	h := newBootstrapHarness(t)
	ctx, cancel := context.WithCancel(context.Background())

	runErr := make(chan error, 1)
	go func() { runErr <- h.subscriber.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	cancel()

	select {
	case err := <-runErr:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestNATSBootstrap_JWKSMismatchReturnsErr(t *testing.T) {
	h := newBootstrapHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- h.subscriber.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	// kid mismatch — controller generated a real kid, we send a bogus one.
	badJWKS := `{"keys":[{"kty":"EC","kid":"not-the-controller-kid","crv":"P-256","x":"AAA","y":"BBB"}]}`
	h.publish(BootstrapSubjectJWKS, BootstrapEnvelope{JWKS: badJWKS})

	select {
	case err := <-runErr:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "JWKS kid")
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return on JWKS mismatch")
	}
}

func TestNATSBootstrap_EmptyEnvelopeFieldsRejected(t *testing.T) {
	h := newBootstrapHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- h.subscriber.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	h.publish(BootstrapSubjectToken, BootstrapEnvelope{})

	select {
	case err := <-runErr:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "token envelope empty")
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return on empty envelope")
	}
}

func TestNATSBootstrap_OneShotIgnoresSubsequentMessages(t *testing.T) {
	h := newBootstrapHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	jwks := string(h.loadControllerJWKS())
	caB64 := base64.StdEncoding.EncodeToString([]byte("ca-pem"))

	runErr := make(chan error, 1)
	go func() { runErr <- h.subscriber.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	// First, send only token so subsequent token publications are dropped.
	h.publish(BootstrapSubjectToken, BootstrapEnvelope{Token: "first-token"})
	time.Sleep(50 * time.Millisecond)
	// Second token publication has a different value; persistToken should not
	// re-encrypt/overwrite because the one-shot map prevents reprocess.
	h.publish(BootstrapSubjectToken, BootstrapEnvelope{Token: "second-token"})

	h.publish(BootstrapSubjectKubeconfig, BootstrapEnvelope{AdminKubeconfig: "kc"})
	h.publish(BootstrapSubjectJWKS, BootstrapEnvelope{JWKS: jwks})
	h.publish(BootstrapSubjectCA, BootstrapEnvelope{CertificateAuthority: caB64})

	select {
	case err := <-runErr:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}

	entry, err := h.kv.Get(NodeTokenKey(bootstrapTestCluster))
	require.NoError(t, err)
	dec, err := handlers_iam.DecryptSecret(string(entry.Value()), bootstrapTestMasterKey)
	require.NoError(t, err)
	assert.Equal(t, "first-token", dec, "second token publication must be dropped")
}

func TestNATSBootstrap_CANotBase64Rejected(t *testing.T) {
	h := newBootstrapHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- h.subscriber.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	h.publish(BootstrapSubjectCA, BootstrapEnvelope{CertificateAuthority: "not!base64@@@"})

	select {
	case err := <-runErr:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CA not base64")
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestPersistWithRetry(t *testing.T) {
	t.Run("succeeds first try", func(t *testing.T) {
		calls := 0
		err := persistWithRetry(func() error { calls++; return nil })
		require.NoError(t, err)
		assert.Equal(t, 1, calls)
	})

	t.Run("retries transient then succeeds", func(t *testing.T) {
		calls := 0
		err := persistWithRetry(func() error {
			calls++
			if calls < 2 {
				return errors.New("transient kv blip")
			}
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, 2, calls)
	})

	t.Run("permanent error returns immediately without retry", func(t *testing.T) {
		calls := 0
		err := persistWithRetry(func() error {
			calls++
			return fmt.Errorf("%w: malformed envelope", errBootstrapPermanent)
		})
		require.ErrorIs(t, err, errBootstrapPermanent)
		assert.Equal(t, 1, calls, "permanent error must not be retried")
	})

	t.Run("transient error exhausts attempts", func(t *testing.T) {
		calls := 0
		err := persistWithRetry(func() error { calls++; return errors.New("always transient") })
		require.Error(t, err)
		assert.Equal(t, bootstrapPersistAttempts, calls)
	})
}

func TestBootstrapPendingKinds(t *testing.T) {
	h := newBootstrapHarness(t)

	meta, err := GetClusterMeta(h.kv, bootstrapTestCluster)
	require.NoError(t, err)

	// Fresh cluster: none of the three artifacts present.
	assert.Equal(t,
		[]string{BootstrapSubjectToken, BootstrapSubjectKubeconfig, BootstrapSubjectCA},
		BootstrapPendingKinds(h.kv, bootstrapTestCluster, meta))

	// Persist token only -> token drops out of pending.
	ct, err := handlers_iam.EncryptSecret("tok", bootstrapTestMasterKey)
	require.NoError(t, err)
	_, err = h.kv.Put(NodeTokenKey(bootstrapTestCluster), []byte(ct))
	require.NoError(t, err)
	assert.Equal(t,
		[]string{BootstrapSubjectKubeconfig, BootstrapSubjectCA},
		BootstrapPendingKinds(h.kv, bootstrapTestCluster, meta))

	// Persist kubeconfig + CA-on-meta -> nothing pending.
	_, err = h.kv.Put(AdminKubeconfigKey(bootstrapTestCluster), []byte(ct))
	require.NoError(t, err)
	meta.CertificateAuthorityB64 = "ca-b64"
	assert.Empty(t, BootstrapPendingKinds(h.kv, bootstrapTestCluster, meta))
}

func TestNATSBootstrap_RunForKindsWaitsOnlyForSubset(t *testing.T) {
	h := newBootstrapHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	caB64 := base64.StdEncoding.EncodeToString([]byte("ca-pem"))

	runErr := make(chan error, 1)
	go func() { runErr <- h.subscriber.RunForKinds(ctx, []string{BootstrapSubjectCA}) }()
	time.Sleep(50 * time.Millisecond)

	// Only publish CA; the subset subscriber must complete without token,
	// kubeconfig, or JWKS ever arriving (they were consumed pre-restart).
	h.publish(BootstrapSubjectCA, BootstrapEnvelope{CertificateAuthority: caB64})

	select {
	case err := <-runErr:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("RunForKinds did not return after CA-only publication")
	}

	meta, err := GetClusterMeta(h.kv, bootstrapTestCluster)
	require.NoError(t, err)
	assert.Equal(t, caB64, meta.CertificateAuthorityB64)
}

func TestNATSBootstrap_RunForKindsRejectsUnknownKind(t *testing.T) {
	h := newBootstrapHarness(t)
	err := h.subscriber.RunForKinds(context.Background(), []string{"bogus-kind"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown bootstrap kind")
}

func TestBootstrapSubject_Shape(t *testing.T) {
	got := BootstrapSubject("111122223333", "alpha", BootstrapSubjectToken)
	assert.Equal(t, "eks.bus.111122223333.alpha.k3s-bootstrap-token", got)
	assert.True(t, strings.HasPrefix(got, "eks.bus."))
}
