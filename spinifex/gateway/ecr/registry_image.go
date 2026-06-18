package gateway_ecr

import (
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecr"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
)

// ErrImageNotFound is returned by the image read/delete helpers when a tag or
// digest does not resolve to a stored manifest.
var ErrImageNotFound = errors.New("image not found")

// ManifestStoreError carries an OCI/HTTP status and code so the OCI PUT path can
// render the v2 error envelope while JSON PutImage maps it to an AWS error.
type ManifestStoreError struct {
	Status int
	Code   string
	Msg    string
}

func (e *ManifestStoreError) Error() string { return e.Msg }

// ImageID pairs a manifest digest with an optional tag.
type ImageID struct {
	Digest string
	Tag    string
}

// ImageRecord is a stored image's metadata, resolved with its tag set.
type ImageRecord struct {
	Digest    string
	Tags      []string
	Size      int64
	PushedAt  time.Time
	MediaType string
}

// StoreManifest validates, stores and (for a tag reference) tags a manifest for
// the given account. It is the shared core of the OCI manifest PUT and the JSON
// PutImage action. ref is a tag or a digest; contentType may be empty, in which
// case it is detected. On a validation/size failure it returns a
// *ManifestStoreError carrying the OCI code.
func (reg *Registry) StoreManifest(account, repo, ref, contentType string, body []byte) (string, error) {
	scoped := reg.forAccount(account)
	if err := scoped.ensureRepo(repo); err != nil {
		return "", err
	}
	if len(body) > maxManifestBytes {
		return "", &ManifestStoreError{http.StatusRequestEntityTooLarge, "MANIFEST_INVALID", "manifest too large"}
	}
	if contentType == "" {
		contentType = detectManifestType(body)
	}

	digest := "sha256:" + hex.EncodeToString(sha256Sum(body))
	if ecr.ValidateDigest(ref) && ref != digest {
		return "", &ManifestStoreError{http.StatusBadRequest, "DIGEST_INVALID", "reference digest does not match content"}
	}

	// On an IMMUTABLE repo, re-tagging an existing tag onto a different digest is
	// rejected before any write so a published tag cannot be silently overwritten.
	if ref != "" && !ecr.ValidateDigest(ref) {
		meta, err := scoped.Meta.GetRepo(account, repo)
		if err != nil {
			return "", err
		}
		if meta.TagMutability() == ecr.TagMutabilityImmutable {
			existing, err := scoped.Meta.GetTag(account, repo, ref)
			switch {
			case err == nil && existing != digest:
				return "", &ManifestStoreError{http.StatusConflict, "TAG_IMMUTABLE", "tag is immutable and already resolves to a different digest"}
			case err != nil && !errors.Is(err, ecr.ErrNotFound):
				return "", err
			}
		}
	}

	children, code, vErr := scoped.validateManifest(repo, contentType, body)
	if vErr != nil {
		return "", &ManifestStoreError{http.StatusBadRequest, code, vErr.Error()}
	}

	if _, err := scoped.Store.PutObject(&s3.PutObjectInput{
		Bucket:      aws.String(scoped.bucket()),
		Key:         aws.String(ecr.ManifestKey(repo, digest)),
		Body:        aws.ReadSeekCloser(strings.NewReader(string(body))),
		ContentType: aws.String(contentType),
	}); err != nil {
		return "", err
	}

	if err := scoped.Meta.PutManifestMeta(account, repo, ecr.ManifestMeta{
		Digest:       digest,
		MediaType:    contentType,
		Size:         int64(len(body)),
		PushedAt:     time.Now().UTC(),
		ChildDigests: children,
	}); err != nil {
		return "", err
	}

	if ref != "" && !ecr.ValidateDigest(ref) {
		if err := scoped.Meta.PutTag(account, repo, ref, digest); err != nil {
			return "", err
		}
	}
	return digest, nil
}

// GetManifest returns the verbatim manifest bytes, media type and digest for a
// tag or digest reference. When acceptedTypes is non-empty the stored media type
// must match one of them; a mismatch yields ErrImageNotFound (Q14). A missing
// image yields ErrImageNotFound.
func (reg *Registry) GetManifest(account, repo, ref string, acceptedTypes []string) ([]byte, string, string, error) {
	scoped := reg.forAccount(account)
	digest, ok := scoped.resolveManifestDigest(repo, ref)
	if !ok {
		return nil, "", "", ErrImageNotFound
	}
	meta, err := scoped.Meta.GetManifestMeta(account, repo, digest)
	if err != nil {
		if errors.Is(err, ecr.ErrNotFound) {
			return nil, "", "", ErrImageNotFound
		}
		return nil, "", "", err
	}
	if len(acceptedTypes) > 0 && !mediaTypeAccepted(acceptedTypes, meta.MediaType) {
		return nil, "", "", ErrImageNotFound
	}
	out, err := scoped.Store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(scoped.bucket()),
		Key:    aws.String(ecr.ManifestKey(repo, digest)),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return nil, "", "", ErrImageNotFound
		}
		return nil, "", "", err
	}
	defer func() { _ = out.Body.Close() }()
	bytes, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, "", "", err
	}
	return bytes, meta.MediaType, digest, nil
}

// ListImages resolves every stored manifest in a repo with its tag set. It is
// the shared source for the ListImages and DescribeImages actions. A missing
// repo yields ecr.ErrNotFound.
func (reg *Registry) ListImages(account, repo string) ([]ImageRecord, error) {
	scoped := reg.forAccount(account)
	if _, err := scoped.Meta.GetRepo(account, repo); err != nil {
		return nil, err
	}

	tags, err := scoped.Meta.ListTags(account, repo)
	if err != nil {
		return nil, err
	}
	tagsByDigest := make(map[string][]string)
	for _, tag := range tags {
		digest, err := scoped.Meta.GetTag(account, repo, tag)
		if err != nil {
			if errors.Is(err, ecr.ErrNotFound) {
				continue
			}
			return nil, err
		}
		tagsByDigest[digest] = append(tagsByDigest[digest], tag)
	}

	// ListManifests yields lookup keys (the KV store tokenizes ':' to '-'); the
	// canonical digest is the one stored in the manifest meta, which is what the
	// tag reverse-index is keyed by. Resolve every record on meta.Digest so the
	// digest form and tag association are correct regardless of store.
	keys, err := scoped.Meta.ListManifests(account, repo)
	if err != nil {
		return nil, err
	}
	records := make([]ImageRecord, 0, len(keys))
	for _, key := range keys {
		meta, err := scoped.Meta.GetManifestMeta(account, repo, key)
		if err != nil {
			if errors.Is(err, ecr.ErrNotFound) {
				continue
			}
			return nil, err
		}
		records = append(records, ImageRecord{
			Digest:    meta.Digest,
			Tags:      tagsByDigest[meta.Digest],
			Size:      meta.Size,
			PushedAt:  meta.PushedAt,
			MediaType: meta.MediaType,
		})
	}
	return records, nil
}

// DeleteImage removes an image by tag and/or digest. A tag deletes only that tag
// pointer; a digest deletes the manifest meta and every tag pointing at it. Blob
// and manifest-object GC is deferred (siv-361). The resolved digest is returned.
// A missing image yields ErrImageNotFound.
func (reg *Registry) DeleteImage(account, repo, tag, digest string) (string, error) {
	scoped := reg.forAccount(account)

	if tag != "" && digest == "" {
		resolved, err := scoped.Meta.GetTag(account, repo, tag)
		if err != nil {
			if errors.Is(err, ecr.ErrNotFound) {
				return "", ErrImageNotFound
			}
			return "", err
		}
		if err := scoped.Meta.DeleteTag(account, repo, tag); err != nil {
			return "", err
		}
		return resolved, nil
	}

	meta, err := scoped.Meta.GetManifestMeta(account, repo, digest)
	if err != nil {
		if errors.Is(err, ecr.ErrNotFound) {
			return "", ErrImageNotFound
		}
		return "", err
	}
	tags, err := scoped.Meta.ListTags(account, repo)
	if err != nil {
		return "", err
	}
	for _, t := range tags {
		d, err := scoped.Meta.GetTag(account, repo, t)
		if err != nil {
			if errors.Is(err, ecr.ErrNotFound) {
				continue
			}
			return "", err
		}
		if d == digest {
			if err := scoped.Meta.DeleteTag(account, repo, t); err != nil {
				return "", err
			}
		}
	}
	if err := scoped.Meta.DeleteManifestMeta(account, repo, digest); err != nil {
		return "", err
	}

	// The KV record is gone, so the image is logically deleted. Predastore
	// reclaim is best-effort: a failure leaks bytes (a capacity nuisance) but
	// does not undo the delete.
	scoped.reclaimManifest(account, repo, digest, meta.ChildDigests)
	return digest, nil
}

// reclaimManifest deletes a manifest's predastore object and any child blob no
// longer referenced by a live manifest in the account pool. It must be called
// after the manifest's KV meta is removed, so referencedDigests excludes it.
func (reg *Registry) reclaimManifest(account, repo, digest string, children []string) {
	if _, err := reg.Store.DeleteObject(&s3.DeleteObjectInput{
		Bucket: aws.String(ecr.AccountBucket(account)),
		Key:    aws.String(ecr.ManifestKey(repo, digest)),
	}); err != nil {
		slog.Warn("ECR/GC: manifest object delete failed", "repo", repo, "digest", digest, "err", err)
	}

	if len(children) == 0 {
		return
	}
	referenced, err := reg.referencedDigests(account)
	if err != nil {
		slog.Warn("ECR/GC: reference scan failed, skipping blob reclaim", "account", account, "err", err)
		return
	}
	for _, child := range children {
		if referenced[child] {
			continue
		}
		if _, err := reg.Store.DeleteObject(&s3.DeleteObjectInput{
			Bucket: aws.String(ecr.AccountBucket(account)),
			Key:    aws.String(ecr.BlobKey(child)),
		}); err != nil {
			slog.Warn("ECR/GC: blob delete failed", "digest", child, "err", err)
		}
	}
}

// referencedDigests returns the set of child digests referenced by every live
// manifest across the account's repos. The account blob pool is shared, so the
// reference set spans all repos, not just the one being mutated.
func (reg *Registry) referencedDigests(account string) (map[string]bool, error) {
	repos, err := reg.Meta.ListRepos(account)
	if err != nil {
		return nil, err
	}
	referenced := make(map[string]bool)
	for _, repo := range repos {
		digests, err := reg.Meta.ListManifests(account, repo)
		if err != nil {
			return nil, err
		}
		for _, key := range digests {
			meta, err := reg.Meta.GetManifestMeta(account, repo, key)
			if err != nil {
				if errors.Is(err, ecr.ErrNotFound) {
					continue
				}
				return nil, err
			}
			for _, child := range meta.ChildDigests {
				referenced[child] = true
			}
		}
	}
	return referenced, nil
}

// mediaTypeAccepted reports whether mediaType is in the accepted set.
func mediaTypeAccepted(accepted []string, mediaType string) bool {
	return slices.Contains(accepted, mediaType)
}
