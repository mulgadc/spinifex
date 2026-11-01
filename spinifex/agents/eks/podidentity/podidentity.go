// Package podidentity implements the pod identity webhook that injects IRSA
// credentials into pods of annotated ServiceAccounts, the way the AWS
// amazon-eks-pod-identity-webhook does, plus the Spinifex STS endpoint and
// gateway CA trust. It depends on no Kubernetes client library: the API server
// speaks to it over AdmissionReview JSON and the cluster is reached through the
// small interfaces below.
package podidentity

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

const (
	// DefaultAnnotationDomain is the annotation domain AWS uses.
	DefaultAnnotationDomain = "eks.amazonaws.com"
	// TokenVolumeName and TokenMountPath are where AWS mounts the projected token.
	TokenVolumeName = "aws-iam-token"                                     //nolint:gosec // volume name, not a credential
	TokenMountPath  = "/var/run/secrets/eks.amazonaws.com/serviceaccount" //nolint:gosec // mount path, not a credential
	TokenPath       = "token"
	// DefaultAudience is the token audience when the ServiceAccount sets none.
	DefaultAudience = "sts.amazonaws.com"
	// MinTokenExpiration and MaxTokenExpiration bound a requested expiry in seconds.
	MinTokenExpiration int64 = 600
	MaxTokenExpiration int64 = 86400
	// DefaultMinTokenExpiration is the low end of the jittered default expiry.
	DefaultMinTokenExpiration int64 = 79200

	// CAVolumeName, CAMountPath and CABundleKey describe the CA bundle ConfigMap
	// mount; CAConfigMapName is the ConfigMap published into every namespace.
	CAConfigMapName = "spinifex-ca-bundle"
	CAVolumeName    = "spinifex-ca-bundle"
	CAMountPath     = "/etc/spinifex/ca-bundle"
	CABundleKey     = "ca.pem"
)

// Reasons logged, with the namespace and pod, when injection is skipped or fails.
const (
	ReasonDecode                = "admission object could not be decoded"
	ReasonServiceAccountLookup  = "serviceaccount lookup failed"
	ReasonServiceAccountMissing = "serviceaccount not found"
	ReasonAlreadyConfigured     = "container already has web identity, region and regional sts settings"
	ReasonSkipContainer         = "container named in skip-containers annotation"
)

var (
	// ErrServiceAccountNotFound is returned by a ServiceAccountGetter when the
	// ServiceAccount does not exist.
	ErrServiceAccountNotFound = errors.New("serviceaccount not found")
	// ErrNotFound, ErrAlreadyExists and ErrConflict are the ConfigMapStore errors.
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrConflict      = errors.New("conflict")
)

// Config is the mutator's configuration.
type Config struct {
	// Region is injected as AWS_REGION and AWS_DEFAULT_REGION when non-empty.
	Region string
	// STSEndpoint is injected as AWS_ENDPOINT_URL_STS when non-empty.
	STSEndpoint string
	// AnnotationDomain defaults to DefaultAnnotationDomain.
	AnnotationDomain string
}

// ServiceAccount is the part of a ServiceAccount the mutator reads.
type ServiceAccount struct {
	Annotations map[string]string
}

// ServiceAccountGetter looks up a ServiceAccount.
type ServiceAccountGetter interface {
	GetServiceAccount(ctx context.Context, namespace, name string) (*ServiceAccount, error)
}

// AdmissionRequest is the part of an AdmissionReview request the mutator reads.
type AdmissionRequest struct {
	UID       string          `json:"uid"`
	Operation string          `json:"operation"`
	Namespace string          `json:"namespace"`
	Object    json.RawMessage `json:"object"`
}

// PatchOp is one RFC 6902 operation.
type PatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// AdmissionResponse is the mutator's answer. Patch is empty when the pod is
// left unchanged. The mutator never denies a pod.
type AdmissionResponse struct {
	UID     string
	Allowed bool
	Patch   []PatchOp
}

// Mutator injects IRSA credentials into pods.
type Mutator struct {
	cfg Config
	sas ServiceAccountGetter
	log *slog.Logger
}

// NewMutator returns a Mutator. A nil log uses slog.Default.
func NewMutator(cfg Config, sas ServiceAccountGetter, log *slog.Logger) *Mutator {
	if log == nil {
		log = slog.Default()
	}
	return &Mutator{cfg: cfg, sas: sas, log: log}
}

// Mutate returns the patch for one pod creation request.
func (m *Mutator) Mutate(_ context.Context, req AdmissionRequest) AdmissionResponse {
	return AdmissionResponse{UID: req.UID, Allowed: true}
}

// ServeHTTP serves an AdmissionReview v1 request.
func (m *Mutator) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// ConfigMap is the part of a ConfigMap the publisher manages.
type ConfigMap struct {
	Namespace string
	Name      string
	Data      map[string]string
	// ResourceVersion is opaque and set by the store.
	ResourceVersion string
}

// ConfigMapStore reads and writes ConfigMaps. Get returns ErrNotFound, Create
// returns ErrAlreadyExists, and Update returns ErrConflict or ErrNotFound.
type ConfigMapStore interface {
	Get(ctx context.Context, namespace, name string) (*ConfigMap, error)
	Create(ctx context.Context, cm ConfigMap) error
	Update(ctx context.Context, cm ConfigMap) error
}

// NamespaceSource lists namespaces and reports ones created later.
type NamespaceSource interface {
	List(ctx context.Context) ([]string, error)
	// Watch sends the name of every namespace created after the call until ctx ends.
	Watch(ctx context.Context) (<-chan string, error)
}

// PublisherConfig configures a Publisher.
type PublisherConfig struct {
	// Bundle is the CA bundle published to every namespace as CABundleKey.
	Bundle []byte
}

// Publisher keeps the CA bundle ConfigMap present in every namespace.
type Publisher struct {
	cfg PublisherConfig
	ns  NamespaceSource
	cms ConfigMapStore
	log *slog.Logger
}

// NewPublisher returns a Publisher. A nil log uses slog.Default.
func NewPublisher(cfg PublisherConfig, ns NamespaceSource, cms ConfigMapStore, log *slog.Logger) *Publisher {
	if log == nil {
		log = slog.Default()
	}
	return &Publisher{cfg: cfg, ns: ns, cms: cms, log: log}
}

// Reconcile makes the CA bundle ConfigMap in one namespace match the bundle.
func (p *Publisher) Reconcile(_ context.Context, _ string) error { return nil }

// Run publishes to every existing namespace, then to each new one, and
// re-checks every namespace each resync interval until ctx ends.
func (p *Publisher) Run(_ context.Context, _ time.Duration) error { return nil }

// WebhookConfig describes the MutatingWebhookConfiguration to render.
type WebhookConfig struct {
	Name string
	// URL is the https URL the API server calls, on 127.0.0.1.
	URL string
	// CA is the PEM the API server trusts for URL.
	CA []byte
}

// RenderWebhookConfiguration returns the MutatingWebhookConfiguration as JSON.
func RenderWebhookConfiguration(_ WebhookConfig) ([]byte, error) { return []byte("{}"), nil }

// BuildCABundle returns the system roots plus the gateway CA as one PEM bundle,
// without duplicate certificates.
func BuildCABundle(_, _ []byte) []byte { return nil }
