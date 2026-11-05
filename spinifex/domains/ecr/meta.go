package ecr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// ErrNotFound is returned by MetaStore reads when the requested record is absent.
var ErrNotFound = errors.New("ecr: metadata record not found")

// ErrConflict signals an optimistic-concurrency clash on a CAS update. Callers
// retry the read-modify-write loop on this error.
var ErrConflict = errors.New("ecr: metadata update conflict")

// Image tag mutability settings. An empty stored value is treated as MUTABLE.
const (
	TagMutabilityMutable   = "MUTABLE"
	TagMutabilityImmutable = "IMMUTABLE"
)

// Repository encryption types. AES256 is the only value CreateRepository
// accepts: predastore seals every stored fragment with AES-256-GCM regardless
// of caller input. KMS is rejected: no customer key material is plumbed here.
const (
	EncryptionTypeAES256 = "AES256"
	EncryptionTypeKMS    = "KMS"
)

// RepoMeta is the per-repository metadata record.
type RepoMeta struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	// ImageTagMutability is MUTABLE or IMMUTABLE; empty == MUTABLE.
	ImageTagMutability string `json:"imageTagMutability,omitempty"`
	// EncryptionType is always AES256 in practice; empty == AES256.
	EncryptionType string `json:"encryptionType,omitempty"`
	// ScanOnPush mirrors imageScanningConfiguration.scanOnPush; unset == false,
	// and no path in this codebase ever sets it true.
	ScanOnPush bool `json:"scanOnPush,omitempty"`
	// Tags is the resource tag map managed by TagResource/UntagResource.
	Tags map[string]string `json:"tags,omitempty"`
}

// TagMutability returns the repo's effective mutability, defaulting an unset
// value to MUTABLE.
func (m RepoMeta) TagMutability() string {
	if m.ImageTagMutability == "" {
		return TagMutabilityMutable
	}
	return m.ImageTagMutability
}

// EncryptionTypeOrDefault returns the repo's effective encryption type,
// defaulting an unset value to AES256.
func (m RepoMeta) EncryptionTypeOrDefault() string {
	if m.EncryptionType == "" {
		return EncryptionTypeAES256
	}
	return m.EncryptionType
}

// ManifestMeta records a stored manifest's properties for shallow validation
// of image indexes and Docker-Content-Digest responses.
type ManifestMeta struct {
	Digest       string    `json:"digest"`
	MediaType    string    `json:"mediaType"`
	Size         int64     `json:"size"`
	PushedAt     time.Time `json:"pushedAt"`
	ChildDigests []string  `json:"childDigests,omitempty"`
	// SubjectDigest and ArtifactType are set only for manifests that declare a
	// subject, and feed the OCI referrers listing for that subject.
	SubjectDigest string            `json:"subjectDigest,omitempty"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// ReferrerDescriptor is one entry of a subject's referrers listing: the OCI
// descriptor of a manifest that names the subject.
type ReferrerDescriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

func (m ManifestMeta) referrerDescriptor() ReferrerDescriptor {
	return ReferrerDescriptor{
		MediaType:    m.MediaType,
		Digest:       m.Digest,
		Size:         m.Size,
		ArtifactType: m.ArtifactType,
		Annotations:  m.Annotations,
	}
}

// UploadState tracks an in-progress chunked blob upload. The sha256 hash state
// is carried as a BinaryMarshaler-marshaled snapshot so PATCH chunks resume the
// running digest without re-reading committed bytes.
type UploadState struct {
	RepoName             string    `json:"repoName"`
	StartedAt            time.Time `json:"startedAt"`
	LastActivity         time.Time `json:"lastActivity"`
	CommittedBytes       int64     `json:"committedBytes"`
	SHA256MarshaledState []byte    `json:"sha256MarshaledState"`
	ExpectedDigest       string    `json:"expectedDigest,omitempty"`
	// BytesKey addresses the object holding all bytes committed so far. Each
	// PATCH writes a fresh key and records it under CAS, so the committed hash
	// and the committed bytes always refer to the same object.
	BytesKey string `json:"bytesKey,omitempty"`
}

// MetaStore is the per-account metadata surface backing the OCI registry. Reads
// return ErrNotFound for missing records; CAS updates return ErrConflict on a
// concurrent revision clash so the caller can retry.
type MetaStore interface {
	PutRepo(ctx context.Context, accountID string, meta RepoMeta) error
	GetRepo(ctx context.Context, accountID, repo string) (RepoMeta, error)
	ListRepos(ctx context.Context, accountID string) ([]string, error)
	DeleteRepo(ctx context.Context, accountID, repo string) error

	ListManifests(ctx context.Context, accountID, repo string) ([]string, error)

	PutRepoPolicy(ctx context.Context, accountID, repo string, policyText []byte) error
	GetRepoPolicy(ctx context.Context, accountID, repo string) ([]byte, error)
	DeleteRepoPolicy(ctx context.Context, accountID, repo string) ([]byte, error)

	PutLifecyclePolicy(ctx context.Context, accountID, repo string, policyText []byte) error
	GetLifecyclePolicy(ctx context.Context, accountID, repo string) ([]byte, error)
	DeleteLifecyclePolicy(ctx context.Context, accountID, repo string) ([]byte, error)

	PutTag(ctx context.Context, accountID, repo, tag, digest string) error
	GetTag(ctx context.Context, accountID, repo, tag string) (string, error)
	DeleteTag(ctx context.Context, accountID, repo, tag string) error
	ListTags(ctx context.Context, accountID, repo string) ([]string, error)

	PutManifestMeta(ctx context.Context, accountID, repo string, meta ManifestMeta) error
	GetManifestMeta(ctx context.Context, accountID, repo, digest string) (ManifestMeta, error)
	DeleteManifestMeta(ctx context.Context, accountID, repo, digest string) error

	// ListReferrers returns the manifests in repo that declare subjectDigest as
	// their subject, ordered by digest. A subject with none yields an empty slice.
	ListReferrers(ctx context.Context, accountID, repo, subjectDigest string) ([]ReferrerDescriptor, error)

	PutUpload(ctx context.Context, accountID, uploadID string, state UploadState) (uint64, error)
	GetUpload(ctx context.Context, accountID, uploadID string) (UploadState, uint64, error)
	UpdateUpload(ctx context.Context, accountID, uploadID string, state UploadState, rev uint64) (uint64, error)
	DeleteUpload(ctx context.Context, accountID, uploadID string) error
}

// KVMetaStore is the JetStream-KV-backed MetaStore. Per-account buckets are
// created lazily on first write and cached per process.
type KVMetaStore struct {
	js      jetstream.JetStream
	mu      sync.Mutex
	buckets map[string]jetstream.KeyValue
}

var _ MetaStore = (*KVMetaStore)(nil)

// NewKVMetaStore constructs a KVMetaStore over the supplied JetStream handle.
func NewKVMetaStore(js jetstream.JetStream) *KVMetaStore {
	return &KVMetaStore{js: js, buckets: make(map[string]jetstream.KeyValue)}
}

func (s *KVMetaStore) bucket(ctx context.Context, accountID string) (jetstream.KeyValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kv, ok := s.buckets[accountID]; ok {
		return kv, nil
	}
	name := KVAccountBucket(accountID)
	kv, err := kvstore.GetOrCreateBucket(ctx, s.js, name, KVBucketAccountHistory)
	if err != nil {
		return nil, fmt.Errorf("ecr: open account bucket %s: %w", name, err)
	}
	s.buckets[accountID] = kv
	return kv, nil
}

func (s *KVMetaStore) PutRepo(ctx context.Context, accountID string, meta RepoMeta) error {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = kv.Put(ctx, KVRepoMetaKey(meta.Name), data)
	return err
}

func (s *KVMetaStore) GetRepo(ctx context.Context, accountID, repo string) (RepoMeta, error) {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return RepoMeta{}, err
	}
	entry, err := kv.Get(ctx, KVRepoMetaKey(repo))
	if err != nil {
		return RepoMeta{}, mapKVErr(err)
	}
	var m RepoMeta
	if err := json.Unmarshal(entry.Value(), &m); err != nil {
		return RepoMeta{}, err
	}
	return m, nil
}

func (s *KVMetaStore) ListRepos(ctx context.Context, accountID string) ([]string, error) {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, err
	}
	var repos []string
	for _, k := range keys {
		if !strings.HasPrefix(k, KVReposPrefix) || !strings.HasSuffix(k, "/meta") {
			continue
		}
		name := trimSuffixMeta(k)
		if name != "" {
			repos = append(repos, name)
		}
	}
	sort.Strings(repos)
	return repos, nil
}

// DeleteRepo removes a repository and cascades its metadata: meta, policy, all
// tags, and all manifest records. Predastore blob garbage collection is out of
// scope here (deferred); only the per-account KV records are removed. Returns
// ErrNotFound when the repository meta is absent.
func (s *KVMetaStore) DeleteRepo(ctx context.Context, accountID, repo string) error {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return err
	}
	if _, err := kv.Get(ctx, KVRepoMetaKey(repo)); err != nil {
		return mapKVErr(err)
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil
		}
		return err
	}
	tagsPrefix, manifestsPrefix, referrersPrefix := KVTagsPrefix(repo), KVManifestsPrefix(repo), KVReferrersPrefix(repo)
	metaKey, policyKey, lifecycleKey := KVRepoMetaKey(repo), KVRepoPolicyKey(repo), KVLifecyclePolicyKey(repo)
	for _, k := range keys {
		if k == metaKey || k == policyKey || k == lifecycleKey ||
			strings.HasPrefix(k, tagsPrefix) || strings.HasPrefix(k, manifestsPrefix) || strings.HasPrefix(k, referrersPrefix) {
			if err := kv.Delete(ctx, k); err != nil {
				return mapKVErr(err)
			}
		}
	}
	return nil
}

func (s *KVMetaStore) ListManifests(ctx context.Context, accountID, repo string) ([]string, error) {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, err
	}
	prefix := KVManifestsPrefix(repo)
	var digests []string
	for _, k := range keys {
		if token, ok := strings.CutPrefix(k, prefix); ok {
			digests = append(digests, token)
		}
	}
	sort.Strings(digests)
	return digests, nil
}

func (s *KVMetaStore) PutRepoPolicy(ctx context.Context, accountID, repo string, policyText []byte) error {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return err
	}
	_, err = kv.Put(ctx, KVRepoPolicyKey(repo), policyText)
	return err
}

func (s *KVMetaStore) GetRepoPolicy(ctx context.Context, accountID, repo string) ([]byte, error) {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	entry, err := kv.Get(ctx, KVRepoPolicyKey(repo))
	if err != nil {
		return nil, mapKVErr(err)
	}
	return entry.Value(), nil
}

func (s *KVMetaStore) DeleteRepoPolicy(ctx context.Context, accountID, repo string) ([]byte, error) {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	// JetStream KV Delete is silently idempotent, so the value is read first to
	// surface a real not-found and return the deleted document to the caller.
	entry, err := kv.Get(ctx, KVRepoPolicyKey(repo))
	if err != nil {
		return nil, mapKVErr(err)
	}
	if err := kv.Delete(ctx, KVRepoPolicyKey(repo)); err != nil {
		return nil, mapKVErr(err)
	}
	return entry.Value(), nil
}

func (s *KVMetaStore) PutLifecyclePolicy(ctx context.Context, accountID, repo string, policyText []byte) error {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return err
	}
	_, err = kv.Put(ctx, KVLifecyclePolicyKey(repo), policyText)
	return err
}

func (s *KVMetaStore) GetLifecyclePolicy(ctx context.Context, accountID, repo string) ([]byte, error) {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	entry, err := kv.Get(ctx, KVLifecyclePolicyKey(repo))
	if err != nil {
		return nil, mapKVErr(err)
	}
	return entry.Value(), nil
}

func (s *KVMetaStore) DeleteLifecyclePolicy(ctx context.Context, accountID, repo string) ([]byte, error) {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	// JetStream KV Delete is silently idempotent, so the value is read first to
	// surface a real not-found and return the deleted document to the caller.
	entry, err := kv.Get(ctx, KVLifecyclePolicyKey(repo))
	if err != nil {
		return nil, mapKVErr(err)
	}
	if err := kv.Delete(ctx, KVLifecyclePolicyKey(repo)); err != nil {
		return nil, mapKVErr(err)
	}
	return entry.Value(), nil
}

func (s *KVMetaStore) PutTag(ctx context.Context, accountID, repo, tag, digest string) error {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return err
	}
	_, err = kv.Put(ctx, KVTagKey(repo, tag), []byte(digest))
	return err
}

func (s *KVMetaStore) GetTag(ctx context.Context, accountID, repo, tag string) (string, error) {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return "", err
	}
	entry, err := kv.Get(ctx, KVTagKey(repo, tag))
	if err != nil {
		return "", mapKVErr(err)
	}
	return string(entry.Value()), nil
}

func (s *KVMetaStore) DeleteTag(ctx context.Context, accountID, repo, tag string) error {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return err
	}
	// JetStream KV Delete is silently idempotent, so existence is checked first
	// to surface a real not-found to the caller.
	if _, err := kv.Get(ctx, KVTagKey(repo, tag)); err != nil {
		return mapKVErr(err)
	}
	if err := kv.Delete(ctx, KVTagKey(repo, tag)); err != nil {
		return mapKVErr(err)
	}
	return nil
}

func (s *KVMetaStore) ListTags(ctx context.Context, accountID, repo string) ([]string, error) {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, err
	}
	prefix := KVTagsPrefix(repo)
	var tags []string
	for _, k := range keys {
		if tag, ok := strings.CutPrefix(k, prefix); ok {
			tags = append(tags, tag)
		}
	}
	sort.Strings(tags)
	return tags, nil
}

func (s *KVMetaStore) PutManifestMeta(ctx context.Context, accountID, repo string, meta ManifestMeta) error {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if _, err = kv.Put(ctx, KVManifestKey(repo, meta.Digest), data); err != nil {
		return err
	}
	if meta.SubjectDigest == "" {
		return nil
	}
	entry, err := json.Marshal(meta.referrerDescriptor())
	if err != nil {
		return err
	}
	_, err = kv.Put(ctx, KVReferrerKey(repo, meta.SubjectDigest, meta.Digest), entry)
	return err
}

func (s *KVMetaStore) ListReferrers(ctx context.Context, accountID, repo, subjectDigest string) ([]ReferrerDescriptor, error) {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return []ReferrerDescriptor{}, nil
		}
		return nil, err
	}
	prefix := KVSubjectReferrersPrefix(repo, subjectDigest)
	out := []ReferrerDescriptor{}
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		entry, err := kv.Get(ctx, k)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return nil, err
		}
		var d ReferrerDescriptor
		if err := json.Unmarshal(entry.Value(), &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Digest < out[j].Digest })
	return out, nil
}

func (s *KVMetaStore) GetManifestMeta(ctx context.Context, accountID, repo, digest string) (ManifestMeta, error) {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return ManifestMeta{}, err
	}
	entry, err := kv.Get(ctx, KVManifestKey(repo, digest))
	if err != nil {
		return ManifestMeta{}, mapKVErr(err)
	}
	var m ManifestMeta
	if err := json.Unmarshal(entry.Value(), &m); err != nil {
		return ManifestMeta{}, err
	}
	return m, nil
}

func (s *KVMetaStore) DeleteManifestMeta(ctx context.Context, accountID, repo, digest string) error {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return err
	}
	entry, err := kv.Get(ctx, KVManifestKey(repo, digest))
	if err != nil {
		return mapKVErr(err)
	}
	var m ManifestMeta
	if err := json.Unmarshal(entry.Value(), &m); err != nil {
		return err
	}
	if m.SubjectDigest != "" {
		if err := kv.Delete(ctx, KVReferrerKey(repo, m.SubjectDigest, digest)); err != nil {
			return mapKVErr(err)
		}
	}
	return kv.Delete(ctx, KVManifestKey(repo, digest))
}

func (s *KVMetaStore) PutUpload(ctx context.Context, accountID, uploadID string, state UploadState) (uint64, error) {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return 0, err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	return kv.Put(ctx, KVUploadKey(uploadID), data)
}

func (s *KVMetaStore) GetUpload(ctx context.Context, accountID, uploadID string) (UploadState, uint64, error) {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return UploadState{}, 0, err
	}
	entry, err := kv.Get(ctx, KVUploadKey(uploadID))
	if err != nil {
		return UploadState{}, 0, mapKVErr(err)
	}
	var st UploadState
	if err := json.Unmarshal(entry.Value(), &st); err != nil {
		return UploadState{}, 0, err
	}
	return st, entry.Revision(), nil
}

func (s *KVMetaStore) UpdateUpload(ctx context.Context, accountID, uploadID string, state UploadState, rev uint64) (uint64, error) {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return 0, err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	newRev, err := kv.Update(ctx, KVUploadKey(uploadID), data, rev)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			return 0, ErrConflict
		}
		return 0, err
	}
	return newRev, nil
}

func (s *KVMetaStore) DeleteUpload(ctx context.Context, accountID, uploadID string) error {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return err
	}
	// JetStream KV Delete is silently idempotent, so existence is checked first
	// to surface a real not-found to the caller.
	if _, err := kv.Get(ctx, KVUploadKey(uploadID)); err != nil {
		return mapKVErr(err)
	}
	if err := kv.Delete(ctx, KVUploadKey(uploadID)); err != nil {
		return mapKVErr(err)
	}
	return nil
}

// mapKVErr normalizes JetStream KV errors to the MetaStore error vocabulary.
func mapKVErr(err error) error {
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return ErrNotFound
	}
	return err
}

// trimSuffixMeta extracts the repo name from a "repos/{name}/meta" key.
func trimSuffixMeta(key string) string {
	name := strings.TrimPrefix(key, KVReposPrefix)
	return strings.TrimSuffix(name, "/meta")
}
