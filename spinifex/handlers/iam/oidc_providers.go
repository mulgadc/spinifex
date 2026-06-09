package handlers_iam

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// Per-account IAM JetStream KV bucket holds resources that are inherently
// account-scoped and not appropriate for the global IAM buckets — currently
// the OIDC identity-provider registry consumed by STS AssumeRoleWithWebIdentity
// and by IAM's CreateOpenIDConnectProvider CRUD.
//
// Lazy creation: callers that read MUST tolerate ErrBucketNotFound (no provider
// has been registered for the account yet); callers that write MUST use
// GetOrCreateIAMAccountBucket so the bucket appears on first use.
const (
	KVBucketIAMAccountPrefix  = "iam-account-"
	KVBucketIAMAccountVersion = 1

	// oidcProvidersKeyPrefix matches the eks-v1.md Q4 layout:
	// "iam-account-{accountID}/oidc-providers/{issuerHash}". Keep the literal
	// here (not strings.Join) so a stray refactor that introduces a different
	// separator fails compile rather than silently writing to a sibling path.
	oidcProvidersKeyPrefix = "oidc-providers/"
)

// IAMAccountBucketName returns the JetStream KV bucket name for the supplied
// AWS account ID.
func IAMAccountBucketName(accountID string) string {
	return KVBucketIAMAccountPrefix + accountID
}

// OIDCProviderKey returns the KV key for an OIDC provider entry. The key is
// derived from the SHA-256 hex digest of the issuer URL — the Mulga
// convention (AWS uses the SHA-1 thumbprint of the IdP root cert; we hash the
// issuer URL because we are also the IdP and have no separate cert chain to
// pin against).
func OIDCProviderKey(issuer string) string {
	sum := sha256.Sum256([]byte(issuer))
	return oidcProvidersKeyPrefix + hex.EncodeToString(sum[:])
}

// OIDCProviderARN composes the AWS-format ARN used as the Federated principal
// value in role trust policies that authorise the named issuer:
//
//	arn:aws:iam::{accountID}:oidc-provider/{issuerHostPath}
//
// issuerHostPath is the issuer URL with the scheme stripped (no leading
// "https://") — matches the AWS convention so customers can paste an existing
// IRSA trust policy unchanged.
func OIDCProviderARN(accountID, issuerHostPath string) string {
	return fmt.Sprintf("arn:aws:iam::%s:oidc-provider/%s", accountID, issuerHostPath)
}

// GetOrCreateIAMAccountBucket opens the per-account IAM bucket, creating it on
// first use. Writers (CreateOpenIDConnectProvider) MUST use this; readers that
// must tolerate a not-yet-created bucket should call js.KeyValue directly and
// treat nats.ErrBucketNotFound as "empty".
func GetOrCreateIAMAccountBucket(js nats.JetStreamContext, accountID string, replicas int) (nats.KeyValue, error) {
	return getOrCreateBucket(js, IAMAccountBucketName(accountID), 1, max(replicas, 1))
}

// OIDCProviderRecord is the stored shape of a registered OIDC identity provider.
// Url carries the full issuer URL including scheme (the exact string the
// cluster apiserver places in the token `iss` claim); the KV key is its hash,
// and the provider ARN is derived by stripping the scheme.
type OIDCProviderRecord struct {
	Url            string   `json:"url"`
	ClientIDList   []string `json:"clientIDList,omitempty"`
	ThumbprintList []string `json:"thumbprintList,omitempty"`
	CreatedAt      string   `json:"createdAt"`
	Tags           []Tag    `json:"tags,omitempty"`
}

// issuerFromOIDCProviderARN reverses OIDCProviderARN: it extracts the
// scheme-less host/path suffix and prepends https:// to reconstruct the issuer
// URL (and therefore its KV key). v1 issuers are always https, so this
// round-trips Create's input.Url.
func issuerFromOIDCProviderARN(arn string) (string, error) {
	_, hostPath, ok := strings.Cut(arn, ":oidc-provider/")
	if !ok {
		return "", errors.New("not an oidc-provider ARN")
	}
	if hostPath == "" {
		return "", errors.New("oidc-provider ARN missing host/path")
	}
	return "https://" + hostPath, nil
}

// validateOIDCProviderURL enforces the v1 issuer shape: a parseable https URL
// with a host. We are the IdP, so no thumbprint/cert-chain validation applies.
func validateOIDCProviderURL(raw string) error {
	if raw == "" {
		return errors.New("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("url scheme must be https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("url missing host")
	}
	return nil
}

func awsStrings(in []*string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != nil {
			out = append(out, *s)
		}
	}
	return out
}

func (s *IAMServiceImpl) CreateOpenIDConnectProvider(accountID string, input *iam.CreateOpenIDConnectProviderInput) (*iam.CreateOpenIDConnectProviderOutput, error) {
	issuer := aws.StringValue(input.Url)
	if err := validateOIDCProviderURL(issuer); err != nil {
		slog.Debug("CreateOpenIDConnectProvider: invalid Url", "url", issuer, "err", err)
		return nil, errors.New(awserrors.ErrorIAMInvalidInput)
	}

	record := OIDCProviderRecord{
		Url:            issuer,
		ClientIDList:   awsStrings(input.ClientIDList),
		ThumbprintList: awsStrings(input.ThumbprintList),
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		Tags:           copyTags(input.Tags),
	}
	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("marshal OIDC provider: %w", err)
	}

	kv, err := GetOrCreateIAMAccountBucket(s.js, accountID, s.replicas)
	if err != nil {
		return nil, fmt.Errorf("open IAM account bucket: %w", err)
	}

	if _, err := kv.Create(OIDCProviderKey(issuer), data); err != nil {
		if errors.Is(err, nats.ErrKeyExists) {
			return nil, errors.New(awserrors.ErrorIAMEntityAlreadyExists)
		}
		return nil, fmt.Errorf("store OIDC provider: %w", err)
	}

	arn := OIDCProviderARN(accountID, stripIssuerScheme(issuer))
	slog.Info("IAM OIDC provider created", "accountID", accountID, "url", issuer, "arn", arn)
	return &iam.CreateOpenIDConnectProviderOutput{
		OpenIDConnectProviderArn: aws.String(arn),
		Tags:                     tagsToSDK(record.Tags),
	}, nil
}

func (s *IAMServiceImpl) GetOpenIDConnectProvider(accountID string, input *iam.GetOpenIDConnectProviderInput) (*iam.GetOpenIDConnectProviderOutput, error) {
	record, err := s.getOIDCProvider(accountID, aws.StringValue(input.OpenIDConnectProviderArn))
	if err != nil {
		return nil, err
	}
	return &iam.GetOpenIDConnectProviderOutput{
		Url:            aws.String(stripIssuerScheme(record.Url)),
		ClientIDList:   aws.StringSlice(record.ClientIDList),
		ThumbprintList: aws.StringSlice(record.ThumbprintList),
		CreateDate:     aws.Time(parseCreatedAt(record.CreatedAt)),
		Tags:           tagsToSDK(record.Tags),
	}, nil
}

func (s *IAMServiceImpl) ListOpenIDConnectProviders(accountID string, _ *iam.ListOpenIDConnectProvidersInput) (*iam.ListOpenIDConnectProvidersOutput, error) {
	out := &iam.ListOpenIDConnectProvidersOutput{
		OpenIDConnectProviderList: []*iam.OpenIDConnectProviderListEntry{},
	}

	kv, err := s.js.KeyValue(IAMAccountBucketName(accountID))
	if err != nil {
		if errors.Is(err, nats.ErrBucketNotFound) {
			return out, nil
		}
		return nil, fmt.Errorf("open IAM account bucket: %w", err)
	}

	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return out, nil
		}
		return nil, fmt.Errorf("list OIDC provider keys: %w", err)
	}

	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, oidcProvidersKeyPrefix) {
			continue
		}
		entry, err := kv.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				continue
			}
			slog.Warn("ListOpenIDConnectProviders: get failed", "key", key, "err", err)
			continue
		}
		var record OIDCProviderRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.Warn("ListOpenIDConnectProviders: unmarshal failed", "key", key, "err", err)
			continue
		}
		out.OpenIDConnectProviderList = append(out.OpenIDConnectProviderList,
			&iam.OpenIDConnectProviderListEntry{
				Arn: aws.String(OIDCProviderARN(accountID, stripIssuerScheme(record.Url))),
			})
	}
	return out, nil
}

func (s *IAMServiceImpl) DeleteOpenIDConnectProvider(accountID string, input *iam.DeleteOpenIDConnectProviderInput) (*iam.DeleteOpenIDConnectProviderOutput, error) {
	arn := aws.StringValue(input.OpenIDConnectProviderArn)
	issuer, err := issuerFromOIDCProviderARN(arn)
	if err != nil {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}

	kv, err := s.js.KeyValue(IAMAccountBucketName(accountID))
	if err != nil {
		if errors.Is(err, nats.ErrBucketNotFound) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return nil, fmt.Errorf("open IAM account bucket: %w", err)
	}

	key := OIDCProviderKey(issuer)
	if _, err := kv.Get(key); err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return nil, fmt.Errorf("get OIDC provider: %w", err)
	}
	if err := kv.Delete(key); err != nil {
		return nil, fmt.Errorf("delete OIDC provider: %w", err)
	}

	slog.Info("IAM OIDC provider deleted", "accountID", accountID, "arn", arn)
	return &iam.DeleteOpenIDConnectProviderOutput{}, nil
}

func (s *IAMServiceImpl) getOIDCProvider(accountID, arn string) (*OIDCProviderRecord, error) {
	issuer, err := issuerFromOIDCProviderARN(arn)
	if err != nil {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	kv, err := s.js.KeyValue(IAMAccountBucketName(accountID))
	if err != nil {
		if errors.Is(err, nats.ErrBucketNotFound) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return nil, fmt.Errorf("open IAM account bucket: %w", err)
	}
	entry, err := kv.Get(OIDCProviderKey(issuer))
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return nil, fmt.Errorf("get OIDC provider: %w", err)
	}
	var record OIDCProviderRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, fmt.Errorf("unmarshal OIDC provider: %w", err)
	}
	return &record, nil
}

// stripIssuerScheme drops the https:// prefix to produce the scheme-less
// host/path form AWS uses in the oidc-provider ARN suffix.
func stripIssuerScheme(issuer string) string {
	return strings.TrimPrefix(issuer, "https://")
}

// tagsToSDK converts stored Tags into the SDK shape used by OIDC-provider
// responses.
func tagsToSDK(tags []Tag) []*iam.Tag {
	if len(tags) == 0 {
		return nil
	}
	out := make([]*iam.Tag, 0, len(tags))
	for _, t := range tags {
		out = append(out, &iam.Tag{Key: aws.String(t.Key), Value: aws.String(t.Value)})
	}
	return out
}
