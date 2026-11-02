package podidentity

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAPI is a TLS API server that requires the webhook's client certificate.
type fakeAPI struct {
	srv     *httptest.Server
	handler http.HandlerFunc
	seenCN  string
}

func newFakeAPI(t *testing.T) (*fakeAPI, *KubeClient) {
	t.Helper()
	f := &fakeAPI{}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.seenCN = r.TLS.PeerCertificates[0].Subject.CommonName
		f.handler(w, r)
	}))
	f.srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS12}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)

	cfg, _ := certTestConfig(t)
	certs, err := EnsureCerts(cfg)
	require.NoError(t, err)
	ca := filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}), 0o644))
	k, err := NewKubeClient(f.srv.URL+"/", ca, certs.ClientCert, certs.ClientKey)
	require.NoError(t, err)
	return f, k
}

func reply(w http.ResponseWriter, code int, body any) {
	w.WriteHeader(code)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func TestKubeClient_PresentsTheClientCertificate(t *testing.T) {
	f, k := newFakeAPI(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, map[string]any{"metadata": map[string]any{}})
	}
	_, err := k.GetServiceAccount(context.Background(), "app", "sa")
	require.NoError(t, err)
	assert.Equal(t, ClientCN, f.seenCN)
}

func TestKubeClient_GetServiceAccount(t *testing.T) {
	f, k := newFakeAPI(t)
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces/app/serviceaccounts/sa":
			reply(w, 200, map[string]any{"metadata": map[string]any{"annotations": map[string]string{"a": "b"}}})
		case "/api/v1/namespaces/app/serviceaccounts/gone":
			reply(w, 404, nil)
		default:
			reply(w, 500, nil)
		}
	}
	sa, err := k.GetServiceAccount(context.Background(), "app", "sa")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "b"}, sa.Annotations)

	_, err = k.GetServiceAccount(context.Background(), "app", "gone")
	assert.ErrorIs(t, err, ErrServiceAccountNotFound)
	_, err = k.GetServiceAccount(context.Background(), "app", "boom")
	assert.Error(t, err)
	assert.NotErrorIs(t, err, ErrServiceAccountNotFound)
}

func TestKubeClient_ConfigMapCalls(t *testing.T) {
	f, k := newFakeAPI(t)
	var gotBody kubeObject
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/app/configmaps/ca":
			reply(w, 200, map[string]any{"metadata": map[string]any{"resourceVersion": "7"}, "data": map[string]string{"k": "v"}})
		case r.Method == http.MethodGet:
			reply(w, 404, nil)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/dup/configmaps":
			reply(w, 409, nil)
		case r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			reply(w, 201, nil)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/namespaces/stale/configmaps/ca":
			reply(w, 409, nil)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/namespaces/gone/configmaps/ca":
			reply(w, 404, nil)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/namespaces/bad/configmaps/ca":
			reply(w, 500, nil)
		default:
			reply(w, 200, nil)
		}
	}
	ctx := context.Background()

	cm, err := k.Get(ctx, "app", "ca")
	require.NoError(t, err)
	assert.Equal(t, "7", cm.ResourceVersion)
	assert.Equal(t, "v", cm.Data["k"])
	_, err = k.Get(ctx, "app", "missing")
	assert.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, k.Create(ctx, ConfigMap{Namespace: "app", Name: "ca", Data: map[string]string{"x": "y"}}))
	assert.Equal(t, "ca", gotBody.Metadata.Name)
	assert.Equal(t, "y", gotBody.Data["x"])
	assert.ErrorIs(t, k.Create(ctx, ConfigMap{Namespace: "dup", Name: "ca"}), ErrAlreadyExists)

	require.NoError(t, k.Update(ctx, ConfigMap{Namespace: "app", Name: "ca"}))
	assert.ErrorIs(t, k.Update(ctx, ConfigMap{Namespace: "stale", Name: "ca"}), ErrConflict)
	assert.ErrorIs(t, k.Update(ctx, ConfigMap{Namespace: "gone", Name: "ca"}), ErrNotFound)
	err = k.Update(ctx, ConfigMap{Namespace: "bad", Name: "ca"})
	assert.Error(t, err)
	assert.NotErrorIs(t, err, ErrConflict)
}

func TestKubeClient_ListNamespaces(t *testing.T) {
	f, k := newFakeAPI(t)
	ok := true
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		if !ok {
			reply(w, 500, nil)
			return
		}
		reply(w, 200, map[string]any{"items": []map[string]any{{"metadata": map[string]string{"name": "a"}}, {"metadata": map[string]string{"name": "b"}}}})
	}
	names, err := k.List(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, names)
	ok = false
	_, err = k.List(context.Background())
	assert.Error(t, err)
}

func TestKubeClient_WatchEmitsAddedNamespacesOnly(t *testing.T) {
	f, k := newFakeAPI(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		enc := json.NewEncoder(w)
		for _, ev := range []struct{ t, n string }{{"ADDED", "one"}, {"MODIFIED", "two"}, {"DELETED", "three"}, {"ADDED", "four"}} {
			_ = enc.Encode(map[string]any{"type": ev.t, "object": map[string]any{"metadata": map[string]string{"name": ev.n}}})
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := k.Watch(ctx)
	require.NoError(t, err)

	got := []string{<-ch, <-ch}
	assert.Equal(t, []string{"one", "four"}, got)
	cancel()
	require.Eventually(t, func() bool {
		select {
		case _, open := <-ch:
			return !open
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond, "the channel closes when the context ends")
}

func TestNewKubeClient_RejectsBadInputs(t *testing.T) {
	cfg, _ := certTestConfig(t)
	certs, err := EnsureCerts(cfg)
	require.NoError(t, err)
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.crt")
	require.NoError(t, os.WriteFile(empty, []byte("not pem"), 0o644))

	_, err = NewKubeClient("https://x", filepath.Join(dir, "absent.crt"), certs.ClientCert, certs.ClientKey)
	assert.Error(t, err)
	_, err = NewKubeClient("https://x", empty, certs.ClientCert, certs.ClientKey)
	assert.Error(t, err)
	_, err = NewKubeClient("https://x", cfg.ServerCACert, filepath.Join(dir, "no.crt"), certs.ClientKey)
	assert.Error(t, err)
}
