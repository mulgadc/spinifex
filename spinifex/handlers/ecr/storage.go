// Package ecr holds the ECR registry handlers and the predastore storage
// layout that backs them. v1 stores everything for one account in a single
// per-account bucket; blobs are content-addressable and deduplicated per
// account, while manifests and tags are indexed per repository.
package ecr

import (
	"fmt"
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
