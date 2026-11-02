package podidentity

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	kubeCallTimeout = 15 * time.Second
	watchRetryDelay = 2 * time.Second
	maxKubeBody     = 4 << 20
)

// KubeClient reaches the Kubernetes API with plain REST. It implements
// ServiceAccountGetter, ConfigMapStore and NamespaceSource using only the
// calls the webhook's ClusterRole grants.
type KubeClient struct {
	base string
	http *http.Client
}

var (
	_ ServiceAccountGetter = (*KubeClient)(nil)
	_ ConfigMapStore       = (*KubeClient)(nil)
	_ NamespaceSource      = (*KubeClient)(nil)
)

// NewKubeClient returns a client for apiServer that trusts caFile and presents
// the client certificate in certFile and keyFile. The pair is re-read on every
// handshake, so a re-issued certificate is used without a restart.
func NewKubeClient(apiServer, caFile, certFile, keyFile string) (*KubeClient, error) {
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read api server ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("api server ca %s holds no certificate", caFile)
	}
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return nil, fmt.Errorf("load client certificate: %w", err)
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			c, err := tls.LoadX509KeyPair(certFile, keyFile)
			return &c, err
		},
	}}
	return &KubeClient{base: strings.TrimSuffix(apiServer, "/"), http: &http.Client{Transport: tr}}, nil
}

type kubeObject struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Metadata   struct {
		Name            string            `json:"name,omitempty"`
		Namespace       string            `json:"namespace,omitempty"`
		ResourceVersion string            `json:"resourceVersion,omitempty"`
		Annotations     map[string]string `json:"annotations,omitempty"`
	} `json:"metadata"`
	Data map[string]string `json:"data,omitempty"`
}

// do sends one request and returns the status. A non-2xx status is returned
// without an error so callers can map the codes they expect.
func (k *KubeClient) do(ctx context.Context, method, path string, body, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, kubeCallTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxKubeBody))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode/100 == 2 && out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

func statusErr(method, path string, code int) error {
	return fmt.Errorf("%s %s: unexpected status %d", method, path, code)
}

// GetServiceAccount implements ServiceAccountGetter.
func (k *KubeClient) GetServiceAccount(ctx context.Context, namespace, name string) (*ServiceAccount, error) {
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/serviceaccounts/" + url.PathEscape(name)
	var o kubeObject
	switch code, err := k.do(ctx, http.MethodGet, path, nil, &o); {
	case err != nil:
		return nil, err
	case code == http.StatusNotFound:
		return nil, ErrServiceAccountNotFound
	case code != http.StatusOK:
		return nil, statusErr(http.MethodGet, path, code)
	}
	return &ServiceAccount{Annotations: o.Metadata.Annotations}, nil
}

func cmPath(namespace, name string) string {
	p := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/configmaps"
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

func cmBody(cm ConfigMap) kubeObject {
	o := kubeObject{APIVersion: "v1", Kind: "ConfigMap", Data: cm.Data}
	o.Metadata.Name, o.Metadata.Namespace, o.Metadata.ResourceVersion = cm.Name, cm.Namespace, cm.ResourceVersion
	return o
}

// Get implements ConfigMapStore.
func (k *KubeClient) Get(ctx context.Context, namespace, name string) (*ConfigMap, error) {
	path := cmPath(namespace, name)
	var o kubeObject
	switch code, err := k.do(ctx, http.MethodGet, path, nil, &o); {
	case err != nil:
		return nil, err
	case code == http.StatusNotFound:
		return nil, ErrNotFound
	case code != http.StatusOK:
		return nil, statusErr(http.MethodGet, path, code)
	}
	return &ConfigMap{Namespace: namespace, Name: name, Data: o.Data, ResourceVersion: o.Metadata.ResourceVersion}, nil
}

// Create implements ConfigMapStore.
func (k *KubeClient) Create(ctx context.Context, cm ConfigMap) error {
	path := cmPath(cm.Namespace, "")
	switch code, err := k.do(ctx, http.MethodPost, path, cmBody(cm), nil); {
	case err != nil:
		return err
	case code == http.StatusConflict:
		return ErrAlreadyExists
	case code/100 != 2:
		return statusErr(http.MethodPost, path, code)
	}
	return nil
}

// Update implements ConfigMapStore.
func (k *KubeClient) Update(ctx context.Context, cm ConfigMap) error {
	path := cmPath(cm.Namespace, cm.Name)
	switch code, err := k.do(ctx, http.MethodPut, path, cmBody(cm), nil); {
	case err != nil:
		return err
	case code == http.StatusConflict:
		return ErrConflict
	case code == http.StatusNotFound:
		return ErrNotFound
	case code/100 != 2:
		return statusErr(http.MethodPut, path, code)
	}
	return nil
}

// List implements NamespaceSource.
func (k *KubeClient) List(ctx context.Context) ([]string, error) {
	const path = "/api/v1/namespaces"
	var l struct {
		Items []kubeObject `json:"items"`
	}
	if code, err := k.do(ctx, http.MethodGet, path, nil, &l); err != nil {
		return nil, err
	} else if code != http.StatusOK {
		return nil, statusErr(http.MethodGet, path, code)
	}
	names := make([]string, 0, len(l.Items))
	for _, it := range l.Items {
		names = append(names, it.Metadata.Name)
	}
	return names, nil
}

// Watch implements NamespaceSource. The stream replays existing namespaces as
// ADDED first, which is harmless, and is re-opened when the server ends it.
// The channel closes only when ctx ends.
func (k *KubeClient) Watch(ctx context.Context) (<-chan string, error) {
	out := make(chan string, 16)
	go func() {
		defer close(out)
		for ctx.Err() == nil {
			k.watchOnce(ctx, out)
			select {
			case <-ctx.Done():
			case <-time.After(watchRetryDelay):
			}
		}
	}()
	return out, nil
}

func (k *KubeClient) watchOnce(ctx context.Context, out chan<- string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.base+"/api/v1/namespaces?watch=true&timeoutSeconds=300", nil)
	if err != nil {
		return
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var ev struct {
			Type   string     `json:"type"`
			Object kubeObject `json:"object"`
		}
		if err := dec.Decode(&ev); err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				return
			}
			return
		}
		if ev.Type != "ADDED" || ev.Object.Metadata.Name == "" {
			continue
		}
		select {
		case out <- ev.Object.Metadata.Name:
		case <-ctx.Done():
			return
		}
	}
}
