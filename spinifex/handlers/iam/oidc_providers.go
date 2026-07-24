package handlers_iam

import (
	"context"
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
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// Per-account IAM bucket holds account-scoped resources (currently the OIDC provider registry).
// Readers must tolerate ErrBucketNotFound; writers must use GetOrCreateIAMAccountBucket.
const (
	KVBucketIAMAccountPrefix  = "iam-account-"
	KVBucketIAMAccountVersion = 1

	oidcProvidersKeyPrefix = "oidc-providers/"
)

// IAMAccountBucketName returns the JetStream KV bucket name for the supplied
// AWS account ID.
func IAMAccountBucketName(accountID string) string {
	return KVBucketIAMAccountPrefix + accountID
}

// OIDCProviderKey returns the KV key for an OIDC provider, derived from the SHA-256 of the issuer URL.
func OIDCProviderKey(issuer string) string {
	sum := sha256.Sum256([]byte(issuer))
	return oidcProvidersKeyPrefix + hex.EncodeToString(sum[:])
}

// OIDCProviderARN returns the arn:aws:iam::{accountID}:oidc-provider/{issuerHostPath} ARN.
// issuerHostPath is the issuer URL with the https:// scheme stripped.
func OIDCProviderARN(accountID, issuerHostPath string) string {
	return fmt.Sprintf("arn:aws:iam::%s:oidc-provider/%s", accountID, issuerHostPath)
}

// GetOrCreateIAMAccountBucket opens the per-account IAM bucket, creating it on first use.
func GetOrCreateIAMAccountBucket(ctx context.Context, js jetstream.JetStream, accountID string, replicas int) (jetstream.KeyValue, error) {
	return kvutil.GetOrCreateBucketWithReplicas(ctx, js, IAMAccountBucketName(accountID), 1, max(replicas, 1))
}

// OIDCProviderRecord is the stored shape of a registered OIDC identity provider.
// Url is the full issuer URL; the KV key is its SHA-256 hash.
type OIDCProviderRecord struct {
	Url            string   `json:"url"`
	ClientIDList   []string `json:"clientIDList,omitempty"`
	ThumbprintList []string `json:"thumbprintList,omitempty"`
	CreatedAt      string   `json:"createdAt"`
	Tags           []Tag    `json:"tags,omitempty"`
}

// issuerFromOIDCProviderARN reverses OIDCProviderARN, reconstructing the full https issuer URL.
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

// validateOIDCProviderURL enforces the issuer shape: a parseable https URL with a non-empty host.
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
	ctx := context.Background()
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

	kv, err := GetOrCreateIAMAccountBucket(ctx, s.js, accountID, s.replicas)
	if err != nil {
		return nil, fmt.Errorf("open IAM account bucket: %w", err)
	}

	if _, err := kv.Create(ctx, OIDCProviderKey(issuer), data); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return nil, errors.New(awserrors.ErrorIAMEntityAlreadyExists)
		}
		return nil, fmt.Errorf("store OIDC provider: %w", err)
	}

	arn := OIDCProviderARN(accountID, strings.TrimPrefix(issuer, "https://"))
	slog.Info("IAM OIDC provider created", "accountID", accountID, "url", issuer, "arn", arn)
	return &iam.CreateOpenIDConnectProviderOutput{
		OpenIDConnectProviderArn: aws.String(arn),
		Tags:                     tagsToSDK(record.Tags),
	}, nil
}

func (s *IAMServiceImpl) GetOpenIDConnectProvider(accountID string, input *iam.GetOpenIDConnectProviderInput) (*iam.GetOpenIDConnectProviderOutput, error) {
	ctx := context.Background()
	record, err := s.getOIDCProvider(ctx, accountID, aws.StringValue(input.OpenIDConnectProviderArn))
	if err != nil {
		return nil, err
	}
	return &iam.GetOpenIDConnectProviderOutput{
		Url:            aws.String(strings.TrimPrefix(record.Url, "https://")),
		ClientIDList:   aws.StringSlice(record.ClientIDList),
		ThumbprintList: aws.StringSlice(record.ThumbprintList),
		CreateDate:     aws.Time(parseCreatedAt(record.CreatedAt)),
		Tags:           tagsToSDK(record.Tags),
	}, nil
}

func (s *IAMServiceImpl) ListOpenIDConnectProviders(accountID string, _ *iam.ListOpenIDConnectProvidersInput) (*iam.ListOpenIDConnectProvidersOutput, error) {
	ctx := context.Background()
	out := &iam.ListOpenIDConnectProvidersOutput{
		OpenIDConnectProviderList: []*iam.OpenIDConnectProviderListEntry{},
	}

	kv, err := s.js.KeyValue(ctx, IAMAccountBucketName(accountID))
	if err != nil {
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return out, nil
		}
		return nil, fmt.Errorf("open IAM account bucket: %w", err)
	}

	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return out, nil
		}
		return nil, fmt.Errorf("list OIDC provider keys: %w", err)
	}

	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, oidcProvidersKeyPrefix) {
			continue
		}
		entry, err := kv.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
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
				Arn: aws.String(OIDCProviderARN(accountID, strings.TrimPrefix(record.Url, "https://"))),
			})
	}
	return out, nil
}

func (s *IAMServiceImpl) DeleteOpenIDConnectProvider(accountID string, input *iam.DeleteOpenIDConnectProviderInput) (*iam.DeleteOpenIDConnectProviderOutput, error) {
	ctx := context.Background()
	arn := aws.StringValue(input.OpenIDConnectProviderArn)
	issuer, err := issuerFromOIDCProviderARN(arn)
	if err != nil {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}

	kv, err := s.js.KeyValue(ctx, IAMAccountBucketName(accountID))
	if err != nil {
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return nil, fmt.Errorf("open IAM account bucket: %w", err)
	}

	key := OIDCProviderKey(issuer)
	if _, err := kv.Get(ctx, key); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return nil, fmt.Errorf("get OIDC provider: %w", err)
	}
	if err := kv.Delete(ctx, key); err != nil {
		return nil, fmt.Errorf("delete OIDC provider: %w", err)
	}

	slog.Info("IAM OIDC provider deleted", "accountID", accountID, "arn", arn)
	return &iam.DeleteOpenIDConnectProviderOutput{}, nil
}

// TagOpenIDConnectProvider upserts tags on an OIDC provider. Blind
// read-modify-write Put like the other writers here (no CAS).
func (s *IAMServiceImpl) TagOpenIDConnectProvider(accountID string, input *iam.TagOpenIDConnectProviderInput) (*iam.TagOpenIDConnectProviderOutput, error) {
	ctx := context.Background()
	if err := validateTags(input.Tags); err != nil {
		return nil, err
	}

	arn := aws.StringValue(input.OpenIDConnectProviderArn)
	record, err := s.getOIDCProvider(ctx, accountID, arn)
	if err != nil {
		return nil, err
	}

	merged := mergeTags(record.Tags, input.Tags)
	if len(merged) > maxTagsPerResource {
		return nil, errors.New(awserrors.ErrorIAMLimitExceeded)
	}
	record.Tags = merged

	if err := s.putOIDCProvider(ctx, accountID, record); err != nil {
		return nil, err
	}

	slog.Info("IAM OIDC provider tagged", "accountID", accountID, "arn", arn)
	return &iam.TagOpenIDConnectProviderOutput{}, nil
}

// UntagOpenIDConnectProvider removes the named tag keys from an OIDC provider;
// unknown keys are a no-op.
func (s *IAMServiceImpl) UntagOpenIDConnectProvider(accountID string, input *iam.UntagOpenIDConnectProviderInput) (*iam.UntagOpenIDConnectProviderOutput, error) {
	ctx := context.Background()
	arn := aws.StringValue(input.OpenIDConnectProviderArn)
	record, err := s.getOIDCProvider(ctx, accountID, arn)
	if err != nil {
		return nil, err
	}

	record.Tags = removeTagKeys(record.Tags, input.TagKeys)

	if err := s.putOIDCProvider(ctx, accountID, record); err != nil {
		return nil, err
	}

	slog.Info("IAM OIDC provider untagged", "accountID", accountID, "arn", arn)
	return &iam.UntagOpenIDConnectProviderOutput{}, nil
}

// ListOpenIDConnectProviderTags returns an OIDC provider's tags. Pagination is
// not implemented: IsTruncated is always false.
func (s *IAMServiceImpl) ListOpenIDConnectProviderTags(accountID string, input *iam.ListOpenIDConnectProviderTagsInput) (*iam.ListOpenIDConnectProviderTagsOutput, error) {
	ctx := context.Background()
	record, err := s.getOIDCProvider(ctx, accountID, aws.StringValue(input.OpenIDConnectProviderArn))
	if err != nil {
		return nil, err
	}
	return &iam.ListOpenIDConnectProviderTagsOutput{
		Tags:        tagsToSDK(record.Tags),
		IsTruncated: aws.Bool(false),
	}, nil
}

// putOIDCProvider overwrites an existing provider record, keyed by its issuer
// URL. The record must have been read from the bucket first.
func (s *IAMServiceImpl) putOIDCProvider(ctx context.Context, accountID string, record *OIDCProviderRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal OIDC provider: %w", err)
	}
	kv, err := s.js.KeyValue(ctx, IAMAccountBucketName(accountID))
	if err != nil {
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return fmt.Errorf("open IAM account bucket: %w", err)
	}
	if _, err := kv.Put(ctx, OIDCProviderKey(record.Url), data); err != nil {
		return fmt.Errorf("store OIDC provider: %w", err)
	}
	return nil
}

func (s *IAMServiceImpl) getOIDCProvider(ctx context.Context, accountID, arn string) (*OIDCProviderRecord, error) {
	issuer, err := issuerFromOIDCProviderARN(arn)
	if err != nil {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	kv, err := s.js.KeyValue(ctx, IAMAccountBucketName(accountID))
	if err != nil {
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return nil, fmt.Errorf("open IAM account bucket: %w", err)
	}
	entry, err := kv.Get(ctx, OIDCProviderKey(issuer))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
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
