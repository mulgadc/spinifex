package gateway_ecr

import (
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/google/uuid"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecr"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
)

// OCI media types recognised by the manifest validator.
const (
	mediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	mediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeOCIManifest        = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeOCIIndex           = "application/vnd.oci.image.index.v1+json"
	defaultManifestContentType  = mediaTypeDockerManifest
)

// maxManifestBytes caps an accepted manifest document. Manifests are small JSON
// blobs; anything larger is rejected before buffering.
const maxManifestBytes = 4 << 20

// Registry serves the OCI Distribution Spec v2 surface (/v2/*). Blob and
// manifest bytes stream straight to an account-scoped predastore bucket via
// Store. Metadata (repos, tags, manifest records, in-progress upload-state CAS)
// goes through Meta, which the gateway reaches over NATS request/reply to the
// daemon that owns the per-account JetStream KV.
type Registry struct {
	Store     objectstore.ObjectStore
	Meta      ecr.MetaStore
	AccountID string

	// buckets caches which account predastore buckets have been provisioned.
	// Shared across per-request Registry copies so concurrent callers converge.
	buckets *bucketCache
}

// bucketCache records, per account, that the predastore bucket exists. Only
// success is cached: a failed ensure is retried on the next request.
type bucketCache struct {
	mu    sync.Mutex
	ready map[string]bool
}

// NewRegistry wires a Registry to its predastore object store and the metadata
// store (a NATS client in production). accountID is the fallback account used
// when a request carries no auth-bridge account (e.g. unit tests).
func NewRegistry(store objectstore.ObjectStore, meta ecr.MetaStore, accountID string) *Registry {
	return &Registry{
		Store:     store,
		Meta:      meta,
		AccountID: accountID,
		buckets:   &bucketCache{ready: make(map[string]bool)},
	}
}

// forAccount returns a shallow Registry copy scoped to account. Store, Meta and
// the bucket cache are shared by pointer; only the request-scoped AccountID
// differs, so every handler method transparently operates on the caller's
// account without threading it through each signature.
func (reg *Registry) forAccount(account string) *Registry {
	cp := *reg
	cp.AccountID = account
	return &cp
}

// ServeHTTP resolves the caller account from the auth-bridge context (falling
// back to the configured default), then dispatches on a per-request scoped copy.
func (reg *Registry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	account := authAccount(r.Context())
	if account == "" {
		account = reg.AccountID
	}
	if account == "" {
		reg.internal(w, "resolve account", errors.New("no authenticated account"))
		return
	}
	reg.forAccount(account).serve(w, r)
}

// serve dispatches a /v2/* request by manually parsing the path: OCI repo names
// contain slashes, so the {name} segment is everything between "/v2/" and the
// trailing "/blobs", "/manifests" or "/tags" marker.
func (reg *Registry) serve(w http.ResponseWriter, r *http.Request) {
	if err := reg.ensureBucket(); err != nil {
		reg.internal(w, "ensure bucket", err)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/v2/")

	if path == "_catalog" {
		reg.handleCatalog(w, r)
		return
	}

	name, kind, ref, ok := splitV2Path(path)
	if !ok {
		WriteError(w, http.StatusNotFound, "NAME_UNKNOWN", "unrecognized registry path")
		return
	}

	if err := ecr.ValidateRepoName(name); err != nil {
		WriteError(w, http.StatusBadRequest, "NAME_INVALID", err.Error())
		return
	}

	switch kind {
	case "blobs":
		reg.routeBlobs(w, r, name, ref)
	case "manifests":
		reg.routeManifests(w, r, name, ref)
	case "tags":
		reg.routeTags(w, r, name, ref)
	default:
		WriteError(w, http.StatusNotFound, "NAME_UNKNOWN", "unrecognized registry path")
	}
}

// splitV2Path parses "{name}/{kind}/{ref...}" where name may contain slashes.
// kind is one of blobs/manifests/tags. ref is the remainder (digest, reference,
// "uploads", "uploads/{uuid}", or "list"). ok is false if no marker is found.
func splitV2Path(path string) (name, kind, ref string, ok bool) {
	for _, marker := range []string{"/blobs/", "/manifests/", "/tags/"} {
		if before, after, found := strings.Cut(path, marker); found {
			kind = strings.Trim(marker, "/")
			return before, kind, after, before != ""
		}
	}
	return "", "", "", false
}

// ---- blobs ----

func (reg *Registry) routeBlobs(w http.ResponseWriter, r *http.Request, name, ref string) {
	switch {
	case ref == "uploads/" || ref == "uploads":
		if r.Method == http.MethodPost {
			reg.startUpload(w, r, name)
			return
		}
	case strings.HasPrefix(ref, "uploads/"):
		uploadID := strings.TrimPrefix(ref, "uploads/")
		switch r.Method {
		case http.MethodPatch:
			reg.patchUpload(w, r, name, uploadID)
			return
		case http.MethodPut:
			reg.finishUpload(w, r, name, uploadID)
			return
		case http.MethodDelete:
			reg.cancelUpload(w, name, uploadID)
			return
		}
	default:
		// ref is a digest.
		switch r.Method {
		case http.MethodHead:
			reg.headBlob(w, name, ref)
			return
		case http.MethodGet:
			reg.getBlob(w, name, ref)
			return
		}
	}
	WriteError(w, http.StatusNotFound, "BLOB_UNKNOWN", "unsupported blob operation")
}

func (reg *Registry) headBlob(w http.ResponseWriter, _, digest string) {
	if !ecr.ValidateDigest(digest) {
		WriteError(w, http.StatusBadRequest, "DIGEST_INVALID", "malformed digest")
		return
	}
	out, err := reg.Store.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(reg.bucket()),
		Key:    aws.String(ecr.BlobKey(digest)),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			WriteError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob not found")
			return
		}
		reg.internal(w, "head blob", err)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(aws.Int64Value(out.ContentLength), 10))
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusOK)
}

func (reg *Registry) getBlob(w http.ResponseWriter, _, digest string) {
	if !ecr.ValidateDigest(digest) {
		WriteError(w, http.StatusBadRequest, "DIGEST_INVALID", "malformed digest")
		return
	}
	out, err := reg.Store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(reg.bucket()),
		Key:    aws.String(ecr.BlobKey(digest)),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			WriteError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob not found")
			return
		}
		reg.internal(w, "get blob", err)
		return
	}
	defer func() { _ = out.Body.Close() }()
	if out.ContentLength != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(aws.Int64Value(out.ContentLength), 10))
	}
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, out.Body); err != nil {
		slog.Error("ECR/OCI: blob stream failed", "err", err)
	}
}

// startUpload begins a blob upload. A ?mount&from cross-repo mount short-circuits
// to 201 when the digest already lives in the account pool; otherwise it opens a
// new chunked upload (202) with a fresh running sha256 state.
func (reg *Registry) startUpload(w http.ResponseWriter, r *http.Request, name string) {
	if err := reg.ensureRepo(name); err != nil {
		reg.internal(w, "ensure repo", err)
		return
	}

	q := r.URL.Query()
	if mountDigest := q.Get("mount"); mountDigest != "" {
		if ecr.ValidateDigest(mountDigest) && reg.blobExists(mountDigest) {
			reg.writeBlobCreated(w, name, mountDigest)
			return
		}
		// Mount miss: fall through to a normal upload start per the spec.
	}

	uploadID := uuid.NewString()
	h := sha256.New()
	state, err := marshalHash(h)
	if err != nil {
		reg.internal(w, "marshal hash", err)
		return
	}
	now := time.Now().UTC()
	if _, err := reg.Meta.PutUpload(reg.AccountID, uploadID, ecr.UploadState{
		RepoName:             name,
		StartedAt:            now,
		LastActivity:         now,
		SHA256MarshaledState: state,
	}); err != nil {
		reg.internal(w, "create upload", err)
		return
	}

	w.Header().Set("Location", uploadPath(name, uploadID))
	w.Header().Set("Range", "0-0")
	w.Header().Set("Docker-Upload-Uuid", uploadID)
	w.WriteHeader(http.StatusAccepted)
}

// patchUpload appends a chunk to an in-progress upload. predastore exposes no
// append primitive, so each PATCH reads the bytes committed by the prior
// revision, concatenates the chunk, and writes the result under a fresh unique
// key. The running sha256 advances from the stored marshaled state. The new
// byte key and hash state are committed together under a single KV CAS at the
// revision read at the top: a concurrent PATCH that loses the CAS gets 409 and
// its freshly-written object is simply orphaned, never referenced. Because the
// winning record names the exact object it hashed, the digest finally verified
// always equals the bytes finally stored.
func (reg *Registry) patchUpload(w http.ResponseWriter, r *http.Request, name, uploadID string) {
	st, rev, err := reg.Meta.GetUpload(reg.AccountID, uploadID)
	if err != nil {
		if errors.Is(err, ecr.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload not found")
			return
		}
		reg.internal(w, "get upload", err)
		return
	}

	chunk, err := io.ReadAll(r.Body)
	if err != nil {
		reg.internal(w, "read chunk", err)
		return
	}

	prior := reg.readUploadBytesAt(st.BytesKey)
	assembled := append(prior, chunk...)
	newKey := ecr.UploadChunkKey(uploadID, uuid.NewString())
	if err := reg.putUploadBytesAt(newKey, assembled); err != nil {
		reg.internal(w, "store chunk", err)
		return
	}

	h, err := unmarshalHash(st.SHA256MarshaledState)
	if err != nil {
		reg.internal(w, "restore hash", err)
		return
	}
	h.Write(chunk)
	marshaled, err := marshalHash(h)
	if err != nil {
		reg.internal(w, "marshal hash", err)
		return
	}

	priorKey := st.BytesKey
	st.CommittedBytes += int64(len(chunk))
	st.SHA256MarshaledState = marshaled
	st.LastActivity = time.Now().UTC()
	st.BytesKey = newKey
	if _, err := reg.Meta.UpdateUpload(reg.AccountID, uploadID, st, rev); err != nil {
		// Lost the CAS or a real failure: drop the object this attempt wrote so
		// it doesn't linger, then surface 409 (the client/registry retries).
		_ = reg.deleteUploadBytesAt(newKey)
		if errors.Is(err, ecr.ErrConflict) {
			WriteError(w, http.StatusConflict, "BLOB_UPLOAD_INVALID", "concurrent upload modification")
			return
		}
		reg.internal(w, "update upload", err)
		return
	}
	// CAS won: the superseded object is no longer referenced.
	if priorKey != "" {
		_ = reg.deleteUploadBytesAt(priorKey)
	}

	w.Header().Set("Location", uploadPath(name, uploadID))
	w.Header().Set("Range", fmt.Sprintf("0-%d", st.CommittedBytes-1))
	w.Header().Set("Docker-Upload-Uuid", uploadID)
	w.WriteHeader(http.StatusAccepted)
}

// finishUpload finalizes an upload. It supports both monolithic PUT (full body
// here) and chunked completion (body empty, bytes already PATCHed). The server
// recomputes sha256 over all bytes and rejects a mismatch with DIGEST_INVALID.
func (reg *Registry) finishUpload(w http.ResponseWriter, r *http.Request, name, uploadID string) {
	digest := r.URL.Query().Get("digest")
	if digest == "" || !ecr.ValidateDigest(digest) {
		WriteError(w, http.StatusBadRequest, "DIGEST_INVALID", "missing or malformed digest")
		return
	}

	st, _, err := reg.Meta.GetUpload(reg.AccountID, uploadID)
	if err != nil {
		if errors.Is(err, ecr.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload not found")
			return
		}
		reg.internal(w, "get upload", err)
		return
	}

	finalChunk, err := io.ReadAll(r.Body)
	if err != nil {
		reg.internal(w, "read body", err)
		return
	}

	h, err := unmarshalHash(st.SHA256MarshaledState)
	if err != nil {
		reg.internal(w, "restore hash", err)
		return
	}

	var assembled []byte
	if len(finalChunk) > 0 {
		assembled = append(reg.readUploadBytesAt(st.BytesKey), finalChunk...)
		h.Write(finalChunk)
		st.CommittedBytes += int64(len(finalChunk))
	} else {
		assembled = reg.readUploadBytesAt(st.BytesKey)
	}

	computed := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if computed != digest {
		reg.cleanupUpload(uploadID)
		WriteError(w, http.StatusBadRequest, "DIGEST_INVALID", "computed digest does not match expected digest")
		return
	}

	// Concurrent-push short-circuit: if the pool already holds this blob, skip
	// the store and just drop the temp upload.
	if !reg.blobExists(digest) {
		if _, err := reg.Store.PutObject(&s3.PutObjectInput{
			Bucket: aws.String(reg.bucket()),
			Key:    aws.String(ecr.BlobKey(digest)),
			Body:   aws.ReadSeekCloser(strings.NewReader(string(assembled))),
		}); err != nil {
			// Finalize failed: drop the temp object and upload record so the
			// uuid is reusable and no partial blob is left assembled.
			reg.cleanupUpload(uploadID)
			reg.internal(w, "store blob", err)
			return
		}
	}

	reg.cleanupUpload(uploadID)
	reg.writeBlobCreated(w, name, digest)
}

// cancelUpload aborts an in-progress upload, deleting both the committed temp
// bytes and the upload record. An unknown uuid is a 404; a successful delete is
// 204 (idempotent — a second cancel sees the record gone and returns 404).
func (reg *Registry) cancelUpload(w http.ResponseWriter, _, uploadID string) {
	st, _, getErr := reg.Meta.GetUpload(reg.AccountID, uploadID)
	err := reg.Meta.DeleteUpload(reg.AccountID, uploadID)
	switch {
	case errors.Is(err, ecr.ErrNotFound):
		WriteError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload not found")
		return
	case err != nil:
		reg.internal(w, "cancel upload", err)
		return
	}
	if getErr == nil && st.BytesKey != "" {
		if delErr := reg.deleteUploadBytesAt(st.BytesKey); delErr != nil {
			slog.Error("ECR/OCI: cancel upload temp cleanup failed", "uploadID", uploadID, "err", delErr)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// cleanupUpload drops the upload record and its committed temp bytes. Used on
// finalize success and on finalize failure so an aborted upload leaves neither
// an orphaned record (which would block the uuid) nor partial blob bytes.
func (reg *Registry) cleanupUpload(uploadID string) {
	st, _, getErr := reg.Meta.GetUpload(reg.AccountID, uploadID)
	if err := reg.Meta.DeleteUpload(reg.AccountID, uploadID); err != nil && !errors.Is(err, ecr.ErrNotFound) {
		slog.Error("ECR/OCI: upload record cleanup failed", "uploadID", uploadID, "err", err)
	}
	if getErr == nil && st.BytesKey != "" {
		if err := reg.deleteUploadBytesAt(st.BytesKey); err != nil {
			slog.Error("ECR/OCI: upload temp cleanup failed", "uploadID", uploadID, "err", err)
		}
	}
}

func (reg *Registry) writeBlobCreated(w http.ResponseWriter, name, digest string) {
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, digest))
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusCreated)
}

// ---- manifests ----

func (reg *Registry) routeManifests(w http.ResponseWriter, r *http.Request, name, reference string) {
	switch r.Method {
	case http.MethodHead:
		reg.headManifest(w, r, name, reference)
	case http.MethodGet:
		reg.getManifest(w, r, name, reference)
	case http.MethodPut:
		reg.putManifest(w, r, name, reference)
	case http.MethodDelete:
		reg.deleteManifest(w, name, reference)
	default:
		WriteError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "unsupported manifest method")
	}
}

// resolveManifestDigest maps a reference (tag or digest) to a stored digest.
func (reg *Registry) resolveManifestDigest(name, reference string) (string, bool) {
	if ecr.ValidateDigest(reference) {
		return reference, true
	}
	digest, err := reg.Meta.GetTag(reg.AccountID, name, reference)
	if err != nil {
		return "", false
	}
	return digest, true
}

func (reg *Registry) headManifest(w http.ResponseWriter, _ *http.Request, name, reference string) {
	digest, ok := reg.resolveManifestDigest(name, reference)
	if !ok {
		WriteError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}
	meta, err := reg.Meta.GetManifestMeta(reg.AccountID, name, digest)
	if err != nil {
		WriteError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Type", meta.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.WriteHeader(http.StatusOK)
}

func (reg *Registry) getManifest(w http.ResponseWriter, r *http.Request, name, reference string) {
	digest, ok := reg.resolveManifestDigest(name, reference)
	if !ok {
		WriteError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}
	meta, err := reg.Meta.GetManifestMeta(reg.AccountID, name, digest)
	if err != nil {
		WriteError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}
	if !acceptsType(r.Header.Get("Accept"), meta.MediaType) {
		WriteError(w, http.StatusNotAcceptable, "MANIFEST_INVALID", "no acceptable manifest media type")
		return
	}
	out, err := reg.Store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(reg.bucket()),
		Key:    aws.String(ecr.ManifestKey(name, digest)),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			WriteError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
			return
		}
		reg.internal(w, "get manifest", err)
		return
	}
	defer func() { _ = out.Body.Close() }()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		reg.internal(w, "read manifest", err)
		return
	}
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Type", meta.MediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		slog.Error("ECR/OCI: manifest write failed", "err", err)
	}
}

// putManifest validates, stores and tags a manifest. Image manifests are checked
// to ensure every referenced blob exists in the pool; image indexes are checked
// to ensure every referenced child manifest exists with a matching mediaType.
func (reg *Registry) putManifest(w http.ResponseWriter, r *http.Request, name, reference string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxManifestBytes+1))
	if err != nil {
		reg.internal(w, "read manifest", err)
		return
	}

	digest, err := reg.StoreManifest(reg.AccountID, name, reference, r.Header.Get("Content-Type"), body)
	if err != nil {
		var mErr *ManifestStoreError
		if errors.As(err, &mErr) {
			WriteError(w, mErr.Status, mErr.Code, mErr.Msg)
			return
		}
		reg.internal(w, "store manifest", err)
		return
	}

	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/manifests/%s", name, digest))
	w.WriteHeader(http.StatusCreated)
}

func (reg *Registry) deleteManifest(w http.ResponseWriter, name, reference string) {
	// Delete by digest removes the manifest record + object and reclaims any
	// orphaned blobs; delete by tag removes only the tag pointer (the untagged
	// image persists, matching AWS).
	if ecr.ValidateDigest(reference) {
		if _, err := reg.DeleteImage(reg.AccountID, name, "", reference); err != nil {
			if errors.Is(err, ErrImageNotFound) {
				WriteError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
				return
			}
			reg.internal(w, "delete manifest", err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	err := reg.Meta.DeleteTag(reg.AccountID, name, reference)
	if err != nil {
		if errors.Is(err, ecr.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
			return
		}
		reg.internal(w, "delete tag", err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// manifestDoc is the subset of an OCI image manifest the validator inspects.
type manifestDoc struct {
	MediaType string `json:"mediaType"`
	Config    struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Layers []struct {
		Digest string `json:"digest"`
	} `json:"layers"`
	Manifests []struct {
		Digest    string `json:"digest"`
		MediaType string `json:"mediaType"`
	} `json:"manifests"`
}

// validateManifest performs type-aware shallow validation, returning the child
// digests recorded in the manifest metadata. For an image manifest every config
// and layer blob must exist; for an index every child manifest must exist with a
// matching stored mediaType.
func (reg *Registry) validateManifest(name, contentType string, body []byte) ([]string, string, error) {
	var doc manifestDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, "MANIFEST_INVALID", fmt.Errorf("manifest is not valid JSON: %w", err)
	}

	switch contentType {
	case mediaTypeDockerManifestList, mediaTypeOCIIndex:
		var children []string
		for _, m := range doc.Manifests {
			if !ecr.ValidateDigest(m.Digest) {
				return nil, "MANIFEST_INVALID", errors.New("index references malformed digest")
			}
			meta, err := reg.Meta.GetManifestMeta(reg.AccountID, name, m.Digest)
			if err != nil {
				return nil, "MANIFEST_BLOB_UNKNOWN", fmt.Errorf("referenced manifest %s not found", m.Digest)
			}
			if m.MediaType != "" && meta.MediaType != m.MediaType {
				return nil, "MANIFEST_INVALID", fmt.Errorf("referenced manifest %s media type mismatch", m.Digest)
			}
			children = append(children, m.Digest)
		}
		return children, "", nil
	default:
		var children []string
		refs := make([]string, 0, len(doc.Layers)+1)
		if doc.Config.Digest != "" {
			refs = append(refs, doc.Config.Digest)
		}
		for _, l := range doc.Layers {
			refs = append(refs, l.Digest)
		}
		for _, d := range refs {
			if !ecr.ValidateDigest(d) {
				return nil, "MANIFEST_INVALID", errors.New("manifest references malformed digest")
			}
			if !reg.blobExists(d) {
				return nil, "MANIFEST_BLOB_UNKNOWN", fmt.Errorf("referenced blob %s not found", d)
			}
			children = append(children, d)
		}
		return children, "", nil
	}
}

// ---- tags ----

func (reg *Registry) routeTags(w http.ResponseWriter, r *http.Request, name, ref string) {
	if r.Method != http.MethodGet || ref != "list" {
		WriteError(w, http.StatusNotFound, "UNSUPPORTED", "unsupported tags operation")
		return
	}
	if _, err := reg.Meta.GetRepo(reg.AccountID, name); err != nil {
		WriteError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository unknown")
		return
	}
	tags, err := reg.Meta.ListTags(reg.AccountID, name)
	if err != nil {
		reg.internal(w, "list tags", err)
		return
	}
	if tags == nil {
		tags = []string{}
	}
	reg.writeJSON(w, http.StatusOK, struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}{Name: name, Tags: tags})
}

// ---- catalog ----

func (reg *Registry) handleCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "unsupported catalog method")
		return
	}
	repos, err := reg.Meta.ListRepos(reg.AccountID)
	if err != nil {
		reg.internal(w, "list repos", err)
		return
	}
	if repos == nil {
		repos = []string{}
	}
	reg.writeJSON(w, http.StatusOK, struct {
		Repositories []string `json:"repositories"`
	}{Repositories: repos})
}

// ---- helpers ----

func (reg *Registry) bucket() string { return ecr.AccountBucket(reg.AccountID) }

// ensureBucket provisions the account's predastore bucket on first use. ECR
// storage is account-managed: the bucket appears transparently rather than
// being created by an operator. Success is cached per account; a backend
// failure is left uncached so the next request retries.
func (reg *Registry) ensureBucket() error {
	reg.buckets.mu.Lock()
	defer reg.buckets.mu.Unlock()
	if reg.buckets.ready[reg.AccountID] {
		return nil
	}
	if err := reg.Store.EnsureBucket(reg.bucket()); err != nil {
		return err
	}
	reg.buckets.ready[reg.AccountID] = true
	return nil
}

func (reg *Registry) ensureRepo(name string) error {
	if _, err := reg.Meta.GetRepo(reg.AccountID, name); err == nil {
		return nil
	} else if !errors.Is(err, ecr.ErrNotFound) {
		return err
	}
	return reg.Meta.PutRepo(reg.AccountID, ecr.RepoMeta{Name: name, CreatedAt: time.Now().UTC()})
}

func (reg *Registry) blobExists(digest string) bool {
	_, err := reg.Store.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(reg.bucket()),
		Key:    aws.String(ecr.BlobKey(digest)),
	})
	return err == nil
}

// readUploadBytesAt returns the bytes at key, or nil for an empty key or a miss
// (a fresh upload has no committed bytes yet).
func (reg *Registry) readUploadBytesAt(key string) []byte {
	if key == "" {
		return nil
	}
	out, err := reg.Store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(reg.bucket()),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil
	}
	return data
}

func (reg *Registry) putUploadBytesAt(key string, data []byte) error {
	_, err := reg.Store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(reg.bucket()),
		Key:    aws.String(key),
		Body:   aws.ReadSeekCloser(strings.NewReader(string(data))),
	})
	return err
}

func (reg *Registry) deleteUploadBytesAt(key string) error {
	if key == "" {
		return nil
	}
	_, err := reg.Store.DeleteObject(&s3.DeleteObjectInput{
		Bucket: aws.String(reg.bucket()),
		Key:    aws.String(key),
	})
	return err
}

func (reg *Registry) writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		reg.internal(w, "marshal json", err)
		return
	}
	w.Header().Set("Content-Type", OCIContentType)
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		slog.Error("ECR/OCI: json write failed", "err", err)
	}
}

func (reg *Registry) internal(w http.ResponseWriter, op string, err error) {
	slog.Error("ECR/OCI: "+op+" failed", "err", err)
	WriteError(w, http.StatusInternalServerError, "UNKNOWN", "internal registry error")
}

func uploadPath(name, uploadID string) string {
	return fmt.Sprintf("/v2/%s/blobs/uploads/%s", name, uploadID)
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// marshalHash snapshots a running sha256 state via its BinaryMarshaler so a
// chunked upload resumes without re-reading committed bytes.
func marshalHash(h hash.Hash) ([]byte, error) {
	m, ok := h.(encoding.BinaryMarshaler)
	if !ok {
		return nil, errors.New("sha256 hash does not support marshaling")
	}
	return m.MarshalBinary()
}

func unmarshalHash(state []byte) (hash.Hash, error) {
	h := sha256.New()
	u, ok := h.(encoding.BinaryUnmarshaler)
	if !ok {
		return nil, errors.New("sha256 hash does not support unmarshaling")
	}
	if err := u.UnmarshalBinary(state); err != nil {
		return nil, err
	}
	return h, nil
}

// acceptsType reports whether an Accept header permits the stored media type. An
// empty Accept (e.g. a bare docker pull) accepts anything.
func acceptsType(accept, mediaType string) bool {
	if strings.TrimSpace(accept) == "" {
		return true
	}
	for part := range strings.SplitSeq(accept, ",") {
		t := strings.TrimSpace(part)
		if i := strings.Index(t, ";"); i >= 0 {
			t = strings.TrimSpace(t[:i])
		}
		if t == "*/*" || t == "application/*" || t == mediaType {
			return true
		}
	}
	return false
}

// detectManifestType infers a media type from the document body when the client
// omits Content-Type, distinguishing an index from a single image manifest.
func detectManifestType(body []byte) string {
	var probe struct {
		MediaType string `json:"mediaType"`
		Manifests []any  `json:"manifests"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		if probe.MediaType != "" {
			return probe.MediaType
		}
		if len(probe.Manifests) > 0 {
			return mediaTypeOCIIndex
		}
	}
	return defaultManifestContentType
}
