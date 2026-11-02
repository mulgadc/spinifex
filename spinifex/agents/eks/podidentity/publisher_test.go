package podidentity

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memStore is an in-memory ConfigMapStore with the API server's create and
// update semantics: Create fails if present, Update needs the current version.
type memStore struct {
	mu      sync.Mutex
	cms     map[string]ConfigMap
	version int
	creates int
	updates int
}

func newMemStore() *memStore { return &memStore{cms: map[string]ConfigMap{}} }

func (s *memStore) Get(_ context.Context, ns, name string) (*ConfigMap, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cm, ok := s.cms[ns+"/"+name]
	if !ok {
		return nil, ErrNotFound
	}
	return &cm, nil
}

func (s *memStore) Create(_ context.Context, cm ConfigMap) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cms[cm.Namespace+"/"+cm.Name]; ok {
		return ErrAlreadyExists
	}
	s.version++
	cm.ResourceVersion = strconv.Itoa(s.version)
	s.cms[cm.Namespace+"/"+cm.Name] = cm
	s.creates++
	return nil
}

func (s *memStore) Update(_ context.Context, cm ConfigMap) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.cms[cm.Namespace+"/"+cm.Name]
	if !ok {
		return ErrNotFound
	}
	if cur.ResourceVersion != cm.ResourceVersion {
		return ErrConflict
	}
	s.version++
	cm.ResourceVersion = strconv.Itoa(s.version)
	s.cms[cm.Namespace+"/"+cm.Name] = cm
	s.updates++
	return nil
}

func (s *memStore) remove(ns, name string) {
	s.mu.Lock()
	delete(s.cms, ns+"/"+name)
	s.mu.Unlock()
}

func (s *memStore) get(ns string) (ConfigMap, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cm, ok := s.cms[ns+"/"+CAConfigMapName]
	return cm, ok
}

// fakeNamespaces lists a fixed set and replays created ones on Watch.
type fakeNamespaces struct {
	mu      sync.Mutex
	names   []string
	watcher chan string
}

func (f *fakeNamespaces) List(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.names...), nil
}

func (f *fakeNamespaces) Watch(context.Context) (<-chan string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watcher = make(chan string, 8)
	return f.watcher, nil
}

func (f *fakeNamespaces) create(name string) {
	f.mu.Lock()
	f.names = append(f.names, name)
	w := f.watcher
	f.mu.Unlock()
	if w != nil {
		w <- name
	}
}

var testBundle = []byte("-----BEGIN CERTIFICATE-----\nQUJD\n-----END CERTIFICATE-----\n")

func TestPublisher_ReconcileCreatesTheBundleConfigMap(t *testing.T) {
	st := newMemStore()
	p := NewPublisher(PublisherConfig{Bundle: testBundle}, &fakeNamespaces{}, st, nil)

	require.NoError(t, p.Reconcile(context.Background(), "team-a"))

	cm, ok := st.get("team-a")
	require.True(t, ok, "no %s ConfigMap in team-a", CAConfigMapName)
	assert.Equal(t, string(testBundle), cm.Data[CABundleKey])
}

func TestPublisher_ReconcileIsIdempotentAndRepairsDrift(t *testing.T) {
	st := newMemStore()
	p := NewPublisher(PublisherConfig{Bundle: testBundle}, &fakeNamespaces{}, st, nil)
	ctx := context.Background()

	require.NoError(t, p.Reconcile(ctx, "team-a"))
	require.NoError(t, p.Reconcile(ctx, "team-a"))
	assert.Equal(t, 1, st.creates)
	assert.Zero(t, st.updates, "an up-to-date ConfigMap must not be rewritten")

	require.NoError(t, st.Update(ctx, ConfigMap{Namespace: "team-a", Name: CAConfigMapName,
		Data: map[string]string{CABundleKey: "tampered"}, ResourceVersion: "1"}))
	require.NoError(t, p.Reconcile(ctx, "team-a"))
	cm, _ := st.get("team-a")
	assert.Equal(t, string(testBundle), cm.Data[CABundleKey])
}

func TestPublisher_RunPublishesToEveryNamespaceIncludingLaterOnes(t *testing.T) {
	st := newMemStore()
	nss := &fakeNamespaces{names: []string{"default", "kube-system", "team-a"}}
	p := NewPublisher(PublisherConfig{Bundle: testBundle}, nss, st, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, time.Hour) }()

	for _, ns := range []string{"default", "kube-system", "team-a"} {
		require.Eventually(t, func() bool { _, ok := st.get(ns); return ok }, 3*time.Second, 10*time.Millisecond, "namespace %s", ns)
	}
	nss.create("created-later")
	require.Eventually(t, func() bool { _, ok := st.get("created-later"); return ok }, 3*time.Second, 10*time.Millisecond)

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

func TestPublisher_RunRecreatesADeletedConfigMap(t *testing.T) {
	st := newMemStore()
	nss := &fakeNamespaces{names: []string{"team-a"}}
	p := NewPublisher(PublisherConfig{Bundle: testBundle}, nss, st, nil)

	ctx := t.Context()
	go func() { _ = p.Run(ctx, 20*time.Millisecond) }()

	require.Eventually(t, func() bool { _, ok := st.get("team-a"); return ok }, 3*time.Second, 10*time.Millisecond)
	st.remove("team-a", CAConfigMapName)
	require.Eventually(t, func() bool { _, ok := st.get("team-a"); return ok }, 3*time.Second, 10*time.Millisecond,
		"the ConfigMap stayed deleted; pods created now would hang on the missing volume")
}

// All three control-plane servers run a publisher, so racing is the normal case.
func TestPublisher_ConcurrentPublishersDoNotFight(t *testing.T) {
	st := newMemStore()
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 3 {
		p := NewPublisher(PublisherConfig{Bundle: testBundle}, &fakeNamespaces{}, st, nil)
		for range 4 {
			wg.Go(func() {
				errs <- p.Reconcile(context.Background(), "team-a")
			})
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err, "losing a create race is not an error")
	}
	cm, ok := st.get("team-a")
	require.True(t, ok)
	assert.Equal(t, string(testBundle), cm.Data[CABundleKey])
	assert.Equal(t, 1, st.creates)
	assert.Zero(t, st.updates, "publishers with the same bundle must not rewrite each other's ConfigMap")
}

func certPEM(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func subjects(t *testing.T, bundle []byte) []string {
	t.Helper()
	var out []string
	for rest := bundle; ; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			return out
		}
		c, err := x509.ParseCertificate(b.Bytes)
		require.NoError(t, err)
		out = append(out, c.Subject.CommonName)
	}
}

func TestBuildCABundle_HoldsTheSystemRootsAndTheGatewayCAOnce(t *testing.T) {
	root1, root2, gw := certPEM(t, "root-1"), certPEM(t, "root-2"), certPEM(t, "gateway")
	system := append(append([]byte{}, root1...), root2...)

	got := subjects(t, BuildCABundle(system, gw))
	assert.ElementsMatch(t, []string{"root-1", "root-2", "gateway"}, got)

	again := subjects(t, BuildCABundle(append(append([]byte{}, system...), gw...), gw))
	assert.ElementsMatch(t, []string{"root-1", "root-2", "gateway"}, again, "a certificate in both inputs is listed once")
}

func TestRenderWebhookConfiguration(t *testing.T) {
	ca := certPEM(t, "k3s-server-ca")
	raw, err := RenderWebhookConfiguration(WebhookConfig{
		Name: "spinifex-pod-identity",
		URL:  "https://127.0.0.1:8445/mutate",
		CA:   ca,
	})
	require.NoError(t, err)

	var cfg struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Webhooks []struct {
			Name                    string   `json:"name"`
			FailurePolicy           string   `json:"failurePolicy"`
			SideEffects             string   `json:"sideEffects"`
			AdmissionReviewVersions []string `json:"admissionReviewVersions"`
			ClientConfig            struct {
				URL      string `json:"url"`
				Service  any    `json:"service"`
				CABundle string `json:"caBundle"`
			} `json:"clientConfig"`
			Rules []struct {
				Operations []string `json:"operations"`
				Resources  []string `json:"resources"`
			} `json:"rules"`
		} `json:"webhooks"`
	}
	require.NoError(t, json.Unmarshal(raw, &cfg))
	assert.Equal(t, "admissionregistration.k8s.io/v1", cfg.APIVersion)
	assert.Equal(t, "MutatingWebhookConfiguration", cfg.Kind)
	assert.Equal(t, "spinifex-pod-identity", cfg.Metadata.Name)

	require.Len(t, cfg.Webhooks, 1)
	w := cfg.Webhooks[0]
	assert.Equal(t, "Ignore", w.FailurePolicy, "an unreachable injector must never block pod creation")
	assert.Equal(t, "None", w.SideEffects)
	assert.Contains(t, w.AdmissionReviewVersions, "v1")
	assert.Nil(t, w.ClientConfig.Service, "a Service reference cannot be routed from the control plane")
	assert.True(t, strings.HasPrefix(w.ClientConfig.URL, "https://127.0.0.1:"), "url %q", w.ClientConfig.URL)
	got, err := base64.StdEncoding.DecodeString(w.ClientConfig.CABundle)
	require.NoError(t, err)
	assert.Equal(t, string(ca), string(got), "caBundle must be the k3s server CA")
	require.Len(t, w.Rules, 1)
	assert.Equal(t, []string{"CREATE"}, w.Rules[0].Operations)
	assert.Equal(t, []string{"pods"}, w.Rules[0].Resources)
}

func TestRenderWebhookConfiguration_RejectsAURLThatIsNotLoopbackHTTPS(t *testing.T) {
	ca := certPEM(t, "k3s-server-ca")
	for _, url := range []string{
		"http://127.0.0.1:8445/mutate",
		"https://10.0.0.5:8445/mutate",
		"https://webhook.kube-system.svc/mutate",
		"",
	} {
		_, err := RenderWebhookConfiguration(WebhookConfig{Name: "n", URL: url, CA: ca})
		assert.Error(t, err, "url %q", url)
	}
	_, err := RenderWebhookConfiguration(WebhookConfig{Name: "n", URL: "https://127.0.0.1:8445/mutate"})
	assert.Error(t, err, "a missing CA must not render a webhook the API server cannot verify")
}
