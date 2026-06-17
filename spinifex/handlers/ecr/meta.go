package ecr

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// ErrNotFound is returned by MetaStore reads when the requested record is absent.
var ErrNotFound = errors.New("ecr: metadata record not found")

// ErrConflict signals an optimistic-concurrency clash on a CAS update. Callers
// retry the read-modify-write loop on this error.
var ErrConflict = errors.New("ecr: metadata update conflict")

// RepoMeta is the per-repository metadata record.
type RepoMeta struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

// ManifestMeta records a stored manifest's properties for shallow validation
// of image indexes and Docker-Content-Digest responses.
type ManifestMeta struct {
	Digest       string    `json:"digest"`
	MediaType    string    `json:"mediaType"`
	Size         int64     `json:"size"`
	PushedAt     time.Time `json:"pushedAt"`
	ChildDigests []string  `json:"childDigests,omitempty"`
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
	PutRepo(accountID string, meta RepoMeta) error
	GetRepo(accountID, repo string) (RepoMeta, error)
	ListRepos(accountID string) ([]string, error)

	PutTag(accountID, repo, tag, digest string) error
	GetTag(accountID, repo, tag string) (string, error)
	DeleteTag(accountID, repo, tag string) error
	ListTags(accountID, repo string) ([]string, error)

	PutManifestMeta(accountID, repo string, meta ManifestMeta) error
	GetManifestMeta(accountID, repo, digest string) (ManifestMeta, error)

	PutUpload(accountID, uploadID string, state UploadState) (uint64, error)
	GetUpload(accountID, uploadID string) (UploadState, uint64, error)
	UpdateUpload(accountID, uploadID string, state UploadState, rev uint64) (uint64, error)
	DeleteUpload(accountID, uploadID string) error
}

// KVMetaStore is the JetStream-KV-backed MetaStore. Per-account buckets are
// created lazily on first write and cached per process.
type KVMetaStore struct {
	js      nats.JetStreamContext
	mu      sync.Mutex
	buckets map[string]nats.KeyValue
}

var _ MetaStore = (*KVMetaStore)(nil)

// NewKVMetaStore constructs a KVMetaStore over the supplied JetStream context.
func NewKVMetaStore(js nats.JetStreamContext) *KVMetaStore {
	return &KVMetaStore{js: js, buckets: make(map[string]nats.KeyValue)}
}

func (s *KVMetaStore) bucket(accountID string) (nats.KeyValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kv, ok := s.buckets[accountID]; ok {
		return kv, nil
	}
	name := KVAccountBucket(accountID)
	kv, err := utils.GetOrCreateKVBucket(s.js, name, KVBucketAccountHistory)
	if err != nil {
		return nil, fmt.Errorf("ecr: open account bucket %s: %w", name, err)
	}
	s.buckets[accountID] = kv
	return kv, nil
}

func (s *KVMetaStore) PutRepo(accountID string, meta RepoMeta) error {
	kv, err := s.bucket(accountID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = kv.Put(KVRepoMetaKey(meta.Name), data)
	return err
}

func (s *KVMetaStore) GetRepo(accountID, repo string) (RepoMeta, error) {
	kv, err := s.bucket(accountID)
	if err != nil {
		return RepoMeta{}, err
	}
	entry, err := kv.Get(KVRepoMetaKey(repo))
	if err != nil {
		return RepoMeta{}, mapKVErr(err)
	}
	var m RepoMeta
	if err := json.Unmarshal(entry.Value(), &m); err != nil {
		return RepoMeta{}, err
	}
	return m, nil
}

func (s *KVMetaStore) ListRepos(accountID string) ([]string, error) {
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
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

func (s *KVMetaStore) PutTag(accountID, repo, tag, digest string) error {
	kv, err := s.bucket(accountID)
	if err != nil {
		return err
	}
	_, err = kv.Put(KVTagKey(repo, tag), []byte(digest))
	return err
}

func (s *KVMetaStore) GetTag(accountID, repo, tag string) (string, error) {
	kv, err := s.bucket(accountID)
	if err != nil {
		return "", err
	}
	entry, err := kv.Get(KVTagKey(repo, tag))
	if err != nil {
		return "", mapKVErr(err)
	}
	return string(entry.Value()), nil
}

func (s *KVMetaStore) DeleteTag(accountID, repo, tag string) error {
	kv, err := s.bucket(accountID)
	if err != nil {
		return err
	}
	// JetStream KV Delete is silently idempotent, so existence is checked first
	// to surface a real not-found to the caller.
	if _, err := kv.Get(KVTagKey(repo, tag)); err != nil {
		return mapKVErr(err)
	}
	if err := kv.Delete(KVTagKey(repo, tag)); err != nil {
		return mapKVErr(err)
	}
	return nil
}

func (s *KVMetaStore) ListTags(accountID, repo string) ([]string, error) {
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
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

func (s *KVMetaStore) PutManifestMeta(accountID, repo string, meta ManifestMeta) error {
	kv, err := s.bucket(accountID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = kv.Put(KVManifestKey(repo, meta.Digest), data)
	return err
}

func (s *KVMetaStore) GetManifestMeta(accountID, repo, digest string) (ManifestMeta, error) {
	kv, err := s.bucket(accountID)
	if err != nil {
		return ManifestMeta{}, err
	}
	entry, err := kv.Get(KVManifestKey(repo, digest))
	if err != nil {
		return ManifestMeta{}, mapKVErr(err)
	}
	var m ManifestMeta
	if err := json.Unmarshal(entry.Value(), &m); err != nil {
		return ManifestMeta{}, err
	}
	return m, nil
}

func (s *KVMetaStore) PutUpload(accountID, uploadID string, state UploadState) (uint64, error) {
	kv, err := s.bucket(accountID)
	if err != nil {
		return 0, err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	return kv.Put(KVUploadKey(uploadID), data)
}

func (s *KVMetaStore) GetUpload(accountID, uploadID string) (UploadState, uint64, error) {
	kv, err := s.bucket(accountID)
	if err != nil {
		return UploadState{}, 0, err
	}
	entry, err := kv.Get(KVUploadKey(uploadID))
	if err != nil {
		return UploadState{}, 0, mapKVErr(err)
	}
	var st UploadState
	if err := json.Unmarshal(entry.Value(), &st); err != nil {
		return UploadState{}, 0, err
	}
	return st, entry.Revision(), nil
}

func (s *KVMetaStore) UpdateUpload(accountID, uploadID string, state UploadState, rev uint64) (uint64, error) {
	kv, err := s.bucket(accountID)
	if err != nil {
		return 0, err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	newRev, err := kv.Update(KVUploadKey(uploadID), data, rev)
	if err != nil {
		if errors.Is(err, nats.ErrKeyExists) {
			return 0, ErrConflict
		}
		return 0, err
	}
	return newRev, nil
}

func (s *KVMetaStore) DeleteUpload(accountID, uploadID string) error {
	kv, err := s.bucket(accountID)
	if err != nil {
		return err
	}
	// JetStream KV Delete is silently idempotent, so existence is checked first
	// to surface a real not-found to the caller.
	if _, err := kv.Get(KVUploadKey(uploadID)); err != nil {
		return mapKVErr(err)
	}
	if err := kv.Delete(KVUploadKey(uploadID)); err != nil {
		return mapKVErr(err)
	}
	return nil
}

// MemoryMetaStore is an in-memory MetaStore used by tests and single-process
// dev runs. It is safe for concurrent use.
type MemoryMetaStore struct {
	mu        sync.Mutex
	repos     map[string]map[string]RepoMeta     // account -> repo -> meta
	tags      map[string]map[string]string       // account -> repo|tag -> digest
	manifests map[string]map[string]ManifestMeta // account -> repo|digest -> meta
	uploads   map[string]map[string]uploadRev    // account -> id -> state+rev
}

type uploadRev struct {
	state UploadState
	rev   uint64
}

var _ MetaStore = (*MemoryMetaStore)(nil)

// NewMemoryMetaStore constructs an empty in-memory MetaStore.
func NewMemoryMetaStore() *MemoryMetaStore {
	return &MemoryMetaStore{
		repos:     make(map[string]map[string]RepoMeta),
		tags:      make(map[string]map[string]string),
		manifests: make(map[string]map[string]ManifestMeta),
		uploads:   make(map[string]map[string]uploadRev),
	}
}

func (m *MemoryMetaStore) PutRepo(accountID string, meta RepoMeta) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.repos[accountID] == nil {
		m.repos[accountID] = make(map[string]RepoMeta)
	}
	m.repos[accountID][meta.Name] = meta
	return nil
}

func (m *MemoryMetaStore) GetRepo(accountID, repo string) (RepoMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.repos[accountID][repo]
	if !ok {
		return RepoMeta{}, ErrNotFound
	}
	return r, nil
}

func (m *MemoryMetaStore) ListRepos(accountID string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for name := range m.repos[accountID] {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func (m *MemoryMetaStore) PutTag(accountID, repo, tag, digest string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tags[accountID] == nil {
		m.tags[accountID] = make(map[string]string)
	}
	m.tags[accountID][repo+"|"+tag] = digest
	return nil
}

func (m *MemoryMetaStore) GetTag(accountID, repo, tag string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.tags[accountID][repo+"|"+tag]
	if !ok {
		return "", ErrNotFound
	}
	return d, nil
}

func (m *MemoryMetaStore) DeleteTag(accountID, repo, tag string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := repo + "|" + tag
	if _, ok := m.tags[accountID][key]; !ok {
		return ErrNotFound
	}
	delete(m.tags[accountID], key)
	return nil
}

func (m *MemoryMetaStore) ListTags(accountID, repo string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.tags[accountID] {
		if r, t, ok := strings.Cut(k, "|"); ok && r == repo {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *MemoryMetaStore) PutManifestMeta(accountID, repo string, meta ManifestMeta) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.manifests[accountID] == nil {
		m.manifests[accountID] = make(map[string]ManifestMeta)
	}
	m.manifests[accountID][repo+"|"+meta.Digest] = meta
	return nil
}

func (m *MemoryMetaStore) GetManifestMeta(accountID, repo, digest string) (ManifestMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	meta, ok := m.manifests[accountID][repo+"|"+digest]
	if !ok {
		return ManifestMeta{}, ErrNotFound
	}
	return meta, nil
}

func (m *MemoryMetaStore) PutUpload(accountID, uploadID string, state UploadState) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.uploads[accountID] == nil {
		m.uploads[accountID] = make(map[string]uploadRev)
	}
	rev := m.uploads[accountID][uploadID].rev + 1
	m.uploads[accountID][uploadID] = uploadRev{state: state, rev: rev}
	return rev, nil
}

func (m *MemoryMetaStore) GetUpload(accountID, uploadID string) (UploadState, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.uploads[accountID][uploadID]
	if !ok {
		return UploadState{}, 0, ErrNotFound
	}
	return u.state, u.rev, nil
}

func (m *MemoryMetaStore) UpdateUpload(accountID, uploadID string, state UploadState, rev uint64) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.uploads[accountID][uploadID]
	if !ok {
		return 0, ErrNotFound
	}
	if u.rev != rev {
		return 0, ErrConflict
	}
	u.state = state
	u.rev = rev + 1
	m.uploads[accountID][uploadID] = u
	return u.rev, nil
}

func (m *MemoryMetaStore) DeleteUpload(accountID, uploadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.uploads[accountID][uploadID]; !ok {
		return ErrNotFound
	}
	delete(m.uploads[accountID], uploadID)
	return nil
}

// mapKVErr normalizes JetStream KV errors to the MetaStore error vocabulary.
func mapKVErr(err error) error {
	if errors.Is(err, nats.ErrKeyNotFound) {
		return ErrNotFound
	}
	return err
}

// trimSuffixMeta extracts the repo name from a "repos/{name}/meta" key.
func trimSuffixMeta(key string) string {
	name := strings.TrimPrefix(key, KVReposPrefix)
	return strings.TrimSuffix(name, "/meta")
}
