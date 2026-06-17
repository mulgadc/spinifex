// Package ecr holds the ECR registry handlers and the predastore storage
// layout that backs them. v1 stores everything for one account in a single
// per-account bucket; blobs are content-addressable and deduplicated per
// account, while manifests and tags are indexed per repository.
package ecr

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// BucketPrefix is the per-account bucket name prefix. The bucket for an account
// is BucketPrefix + accountID (e.g. "ecr-000000000000"), lazily created on the
// first CreateRepository for that account.
const BucketPrefix = "ecr-"

// AccountBucket returns the predastore bucket name holding all ECR objects for
// the given account.
func AccountBucket(accountID string) string {
	return BucketPrefix + accountID
}

// Object-key layout within an account bucket. These are the canonical key
// shapes every ECR storage path must use. Blobs are pooled per account for
// dedup; manifests and tags are indexed per repository.
const (
	// repoMetaKeyFmt is the repo config object (scan-on-push, tag mutability).
	repoMetaKeyFmt = "repos/%s/_meta.json"
	// tagKeyFmt maps a tag to a digest pointer (body is "sha256:...").
	tagKeyFmt = "repos/%s/manifests/by-tag/%s"
	// manifestKeyFmt holds manifest JSON keyed by its own digest.
	manifestKeyFmt = "repos/%s/manifests/by-digest/%s"
	// indexKeyFmt is the rolling tag->digest index used by ListImages.
	indexKeyFmt = "repos/%s/manifests/index.json"
	// blobKeyFmt is the account-wide blob pool, two-char hex shard.
	blobKeyFmt = "blobs/%s/%s"
	// uploadKeyFmt holds in-progress chunked upload bytes.
	uploadKeyFmt = "uploads/%s"
	// uploadChunkKeyFmt holds a single revision's assembled bytes under a unique
	// per-write token, so a CAS-winning PATCH always points at exactly the bytes
	// it hashed (a losing concurrent write lands on a different, orphaned token).
	uploadChunkKeyFmt = "uploads/%s/%s"
)

// RepoMetaKey returns the key for a repository's config object.
func RepoMetaKey(repo string) string { return fmt.Sprintf(repoMetaKeyFmt, repo) }

// TagKey returns the key for a tag's digest pointer.
func TagKey(repo, tag string) string { return fmt.Sprintf(tagKeyFmt, repo, tag) }

// ManifestKey returns the key for a manifest stored by its digest.
func ManifestKey(repo, digest string) string { return fmt.Sprintf(manifestKeyFmt, repo, digest) }

// IndexKey returns the key for a repository's tag->digest index.
func IndexKey(repo string) string { return fmt.Sprintf(indexKeyFmt, repo) }

// UploadKey returns the key for an in-progress upload's bytes.
func UploadKey(uploadID string) string { return fmt.Sprintf(uploadKeyFmt, uploadID) }

// UploadChunkKey returns the key for one revision's assembled upload bytes,
// addressed by a unique per-write token.
func UploadChunkKey(uploadID, token string) string {
	return fmt.Sprintf(uploadChunkKeyFmt, uploadID, token)
}

// KV bucket layout for per-account ECR metadata. The bucket name is
// KVBucketAccountPrefix + accountID (e.g. "ecr-account-000000000000"), created
// lazily on the first repo create or push for that account.
const (
	KVBucketAccountPrefix  = "ecr-account-"
	KVBucketAccountVersion = 1
	KVBucketAccountHistory = 1
)

// KVAccountBucket returns the per-account JetStream KV bucket name.
func KVAccountBucket(accountID string) string {
	return KVBucketAccountPrefix + accountID
}

// KV key-path helpers for per-account metadata.
//
//	repos/{name}/meta
//	repos/{name}/tags/{tag}
//	repos/{name}/manifests/{digest}
//	uploads/{uuid}
const (
	kvRepoMetaKeyFmt = "repos/%s/meta"
	kvTagsPrefixFmt  = "repos/%s/tags/"
	kvTagKeyFmt      = "repos/%s/tags/%s"
	kvManifestKeyFmt = "repos/%s/manifests/%s"
	kvReposPrefix    = "repos/"
	kvUploadKeyFmt   = "uploads/%s"
)

// KVRepoMetaKey returns the KV key for a repository's meta record.
func KVRepoMetaKey(repo string) string { return fmt.Sprintf(kvRepoMetaKeyFmt, repo) }

// KVTagsPrefix returns the KV key prefix enumerating a repository's tags.
func KVTagsPrefix(repo string) string { return fmt.Sprintf(kvTagsPrefixFmt, repo) }

// KVTagKey returns the KV key for a single tag record.
func KVTagKey(repo, tag string) string { return fmt.Sprintf(kvTagKeyFmt, repo, tag) }

// KVManifestKey returns the KV key for a manifest metadata record. The digest
// (which contains ':') is sanitized to a KV-safe token via DigestToken.
func KVManifestKey(repo, digest string) string {
	return fmt.Sprintf(kvManifestKeyFmt, repo, DigestToken(digest))
}

// KVReposPrefix is the prefix under which every repo meta key lives. Used by
// the catalog listing to enumerate account repositories.
const KVReposPrefix = kvReposPrefix

// KVUploadKey returns the KV key for an in-progress upload's state record.
func KVUploadKey(uploadID string) string { return fmt.Sprintf(kvUploadKeyFmt, uploadID) }

// DigestToken maps a content digest ("sha256:<hex>") to a KV-key-safe token by
// replacing the ':' separator, which JetStream KV keys disallow.
func DigestToken(digest string) string {
	return strings.ReplaceAll(digest, ":", "-")
}

// repoNameRe is the OCI Distribution repository-name grammar. Compiled once.
var repoNameRe = regexp.MustCompile(`^(?:[a-z0-9]+(?:[._-][a-z0-9]+)*/)*[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// ValidateRepoName enforces the OCI repository-name grammar and 2-256 length
// bound. It returns nil for a valid name and a descriptive error otherwise.
func ValidateRepoName(name string) error {
	if len(name) < 2 || len(name) > 256 {
		return errors.New("repository name must be 2-256 characters")
	}
	if !repoNameRe.MatchString(name) {
		return errors.New("repository name does not match the required format")
	}
	return nil
}

// digestRe matches an OCI content digest ("sha256:<64 hex>").
var digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// ValidateDigest reports whether s is a well-formed sha256 content digest.
func ValidateDigest(s string) bool {
	return digestRe.MatchString(s)
}

// BlobKey returns the account-pool key for a blob digest ("sha256:<hex>"). The
// two-char shard is taken from the first hex bytes after the "sha256:" prefix,
// matching the on-disk layout used by upstream OCI registries.
func BlobKey(digest string) string {
	hex := digest
	if _, after, found := strings.Cut(digest, ":"); found {
		hex = after
	}
	shard := hex
	if len(hex) >= 2 {
		shard = hex[:2]
	}
	return fmt.Sprintf(blobKeyFmt, shard, digest)
}
