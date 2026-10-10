package ecr

import "context"

// NATS subjects for the daemon-served ECR metadata surface. The daemon owns the
// JetStream KV; the gateway is a request/reply client. Blob and manifest bytes
// do NOT travel these subjects — only metadata records and upload-state CAS.
const (
	SubjectRepoCreate   = "ecr.repo.create"
	SubjectRepoDescribe = "ecr.repo.describe"
	SubjectRepoList     = "ecr.repo.list"
	SubjectRepoDelete   = "ecr.repo.delete"

	SubjectPolicyPut    = "ecr.policy.put"
	SubjectPolicyGet    = "ecr.policy.get"
	SubjectPolicyDelete = "ecr.policy.delete"

	SubjectLifecyclePut    = "ecr.lifecycle.put"
	SubjectLifecycleGet    = "ecr.lifecycle.get"
	SubjectLifecycleDelete = "ecr.lifecycle.delete"

	SubjectTagPut    = "ecr.tag.put"
	SubjectTagGet    = "ecr.tag.get"
	SubjectTagList   = "ecr.tag.list"
	SubjectTagDelete = "ecr.tag.delete"

	SubjectManifestPut      = "ecr.manifest.put"
	SubjectManifestDescribe = "ecr.manifest.describe"
	SubjectManifestList     = "ecr.manifest.list"
	SubjectManifestDelete   = "ecr.manifest.delete"

	SubjectUploadCreate = "ecr.upload.create"
	SubjectUploadGet    = "ecr.upload.get"
	SubjectUploadUpdate = "ecr.upload.update"
	SubjectUploadDelete = "ecr.upload.delete"
)

// Request/response envelopes for the metadata surface. Absent records and CAS
// conflicts are reported via the Found/Conflict flags rather than transport
// errors, so they round-trip a NATS reply that only carries AWS error codes.

// RepoCreateRequest is the payload for SubjectRepoCreate, carrying the repository record to store.
type RepoCreateRequest struct {
	Meta RepoMeta `json:"meta"`
}

// RepoCreateResponse is the empty reply to SubjectRepoCreate; failures arrive as an AWS error code.
type RepoCreateResponse struct{}

// RepoDescribeRequest is the payload for SubjectRepoDescribe, naming the repository to look up.
type RepoDescribeRequest struct {
	Repo string `json:"repo"`
}

// RepoDescribeResponse is the reply to SubjectRepoDescribe. Found is false, and Meta zero, when the
// repository does not exist in the caller's account.
type RepoDescribeResponse struct {
	Found bool     `json:"found"`
	Meta  RepoMeta `json:"meta"`
}

// RepoListRequest is the empty payload for SubjectRepoList; the account comes from the NATS header.
type RepoListRequest struct{}

// RepoListResponse is the reply to SubjectRepoList, holding the names of every repository in the account.
type RepoListResponse struct {
	Repos []string `json:"repos"`
}

// RepoDeleteRequest is the payload for SubjectRepoDelete, naming the repository record to remove.
type RepoDeleteRequest struct {
	Repo string `json:"repo"`
}

// RepoDeleteResponse is the reply to SubjectRepoDelete. Found is false when there was no repository to delete.
type RepoDeleteResponse struct {
	Found bool `json:"found"`
}

// PolicyPutRequest is the payload for SubjectPolicyPut, carrying the raw JSON repository policy that
// SetRepositoryPolicy stores for Repo.
type PolicyPutRequest struct {
	Repo       string `json:"repo"`
	PolicyText []byte `json:"policyText"`
}

// PolicyPutResponse is the empty reply to SubjectPolicyPut; failures arrive as an AWS error code.
type PolicyPutResponse struct{}

// PolicyGetRequest is the payload for SubjectPolicyGet, naming the repository whose policy to read.
type PolicyGetRequest struct {
	Repo string `json:"repo"`
}

// PolicyGetResponse is the reply to SubjectPolicyGet. Found is false when the repository has no
// policy, which the gateway maps to RepositoryPolicyNotFoundException.
type PolicyGetResponse struct {
	Found      bool   `json:"found"`
	PolicyText []byte `json:"policyText"`
}

// PolicyDeleteRequest is the payload for SubjectPolicyDelete, naming the repository whose policy to remove.
type PolicyDeleteRequest struct {
	Repo string `json:"repo"`
}

// PolicyDeleteResponse is the reply to SubjectPolicyDelete. It returns the deleted policy text, as
// DeleteRepositoryPolicy does, and Found is false when no policy was set.
type PolicyDeleteResponse struct {
	Found      bool   `json:"found"`
	PolicyText []byte `json:"policyText"`
}

// LifecyclePutRequest is the payload for SubjectLifecyclePut, carrying the raw JSON lifecycle policy
// that PutLifecyclePolicy stores for Repo.
type LifecyclePutRequest struct {
	Repo       string `json:"repo"`
	PolicyText []byte `json:"policyText"`
}

// LifecyclePutResponse is the empty reply to SubjectLifecyclePut; failures arrive as an AWS error code.
type LifecyclePutResponse struct{}

// LifecycleGetRequest is the payload for SubjectLifecycleGet, naming the repository whose lifecycle policy to read.
type LifecycleGetRequest struct {
	Repo string `json:"repo"`
}

// LifecycleGetResponse is the reply to SubjectLifecycleGet. Found is false when the repository has no
// lifecycle policy.
type LifecycleGetResponse struct {
	Found      bool   `json:"found"`
	PolicyText []byte `json:"policyText"`
}

// LifecycleDeleteRequest is the payload for SubjectLifecycleDelete, naming the repository whose
// lifecycle policy to remove.
type LifecycleDeleteRequest struct {
	Repo string `json:"repo"`
}

// LifecycleDeleteResponse is the reply to SubjectLifecycleDelete. It returns the deleted policy text,
// as DeleteLifecyclePolicy does, and Found is false when no policy was set.
type LifecycleDeleteResponse struct {
	Found      bool   `json:"found"`
	PolicyText []byte `json:"policyText"`
}

// TagPutRequest is the payload for SubjectTagPut, pointing image tag Tag in Repo at manifest Digest.
type TagPutRequest struct {
	Repo   string `json:"repo"`
	Tag    string `json:"tag"`
	Digest string `json:"digest"`
}

// TagPutResponse is the empty reply to SubjectTagPut; failures arrive as an AWS error code.
type TagPutResponse struct{}

// TagGetRequest is the payload for SubjectTagGet, naming the image tag to resolve.
type TagGetRequest struct {
	Repo string `json:"repo"`
	Tag  string `json:"tag"`
}

// TagGetResponse is the reply to SubjectTagGet, holding the manifest digest the tag points at. Found
// is false when the tag does not exist.
type TagGetResponse struct {
	Found  bool   `json:"found"`
	Digest string `json:"digest"`
}

// TagListRequest is the payload for SubjectTagList, naming the repository whose image tags to list.
type TagListRequest struct {
	Repo string `json:"repo"`
}

// TagListResponse is the reply to SubjectTagList, holding every image tag in the repository.
type TagListResponse struct {
	Tags []string `json:"tags"`
}

// TagDeleteRequest is the payload for SubjectTagDelete, naming the image tag to remove.
type TagDeleteRequest struct {
	Repo string `json:"repo"`
	Tag  string `json:"tag"`
}

// TagDeleteResponse is the reply to SubjectTagDelete. Found is false when the tag did not exist.
type TagDeleteResponse struct {
	Found bool `json:"found"`
}

// ManifestPutRequest is the payload for SubjectManifestPut, carrying the manifest metadata record to
// store; the manifest bytes themselves never travel NATS.
type ManifestPutRequest struct {
	Repo string       `json:"repo"`
	Meta ManifestMeta `json:"meta"`
}

// ManifestPutResponse is the empty reply to SubjectManifestPut; failures arrive as an AWS error code.
type ManifestPutResponse struct{}

// ManifestDescribeRequest is the payload for SubjectManifestDescribe, naming a manifest by repository and digest.
type ManifestDescribeRequest struct {
	Repo   string `json:"repo"`
	Digest string `json:"digest"`
}

// ManifestDescribeResponse is the reply to SubjectManifestDescribe. Found is false when no manifest
// with that digest is recorded.
type ManifestDescribeResponse struct {
	Found bool         `json:"found"`
	Meta  ManifestMeta `json:"meta"`
}

// ManifestListRequest is the payload for SubjectManifestList, naming the repository whose manifests to list.
type ManifestListRequest struct {
	Repo string `json:"repo"`
}

// ManifestListResponse is the reply to SubjectManifestList, holding the digest of every manifest in the repository.
type ManifestListResponse struct {
	Digests []string `json:"digests"`
}

// ManifestDeleteRequest is the payload for SubjectManifestDelete, naming the manifest metadata record to remove.
type ManifestDeleteRequest struct {
	Repo   string `json:"repo"`
	Digest string `json:"digest"`
}

// ManifestDeleteResponse is the reply to SubjectManifestDelete. Found is false when the manifest was not recorded.
type ManifestDeleteResponse struct {
	Found bool `json:"found"`
}

// UploadCreateRequest is the payload for SubjectUploadCreate, carrying the initial state of a new
// blob upload session.
type UploadCreateRequest struct {
	UploadID string      `json:"uploadID"`
	State    UploadState `json:"state"`
}

// UploadCreateResponse is the reply to SubjectUploadCreate, holding the KV revision the caller must
// pass to the first UploadUpdateRequest.
type UploadCreateResponse struct {
	Revision uint64 `json:"revision"`
}

// UploadGetRequest is the payload for SubjectUploadGet, naming the upload session to read.
type UploadGetRequest struct {
	UploadID string `json:"uploadID"`
}

// UploadGetResponse is the reply to SubjectUploadGet, holding the upload state and its current KV
// revision. Found is false when the session does not exist.
type UploadGetResponse struct {
	Found    bool        `json:"found"`
	State    UploadState `json:"state"`
	Revision uint64      `json:"revision"`
}

// UploadUpdateRequest is the payload for SubjectUploadUpdate, a compare-and-swap that writes State only
// if the stored record is still at Revision.
type UploadUpdateRequest struct {
	UploadID string      `json:"uploadID"`
	State    UploadState `json:"state"`
	Revision uint64      `json:"revision"`
}

// UploadUpdateResponse is the reply to SubjectUploadUpdate. Conflict is true when the stored revision
// moved on; otherwise Revision is the new revision. Found is false for an unknown session.
type UploadUpdateResponse struct {
	Found    bool   `json:"found"`
	Conflict bool   `json:"conflict"`
	Revision uint64 `json:"revision"`
}

// UploadDeleteRequest is the payload for SubjectUploadDelete, naming the upload session to remove.
type UploadDeleteRequest struct {
	UploadID string `json:"uploadID"`
}

// UploadDeleteResponse is the reply to SubjectUploadDelete. Found is false when the session did not exist.
type UploadDeleteResponse struct {
	Found bool `json:"found"`
}

// MetaService is the daemon-side metadata surface. Each method takes the
// account ID (carried in the NATS header by the gateway) and returns a typed
// response. Absence and CAS conflicts are encoded in the response, not as a
// transport error, so they survive the AWS-error-code-only NATS reply.
type MetaService interface {
	RepoCreate(ctx context.Context, req *RepoCreateRequest, accountID string) (*RepoCreateResponse, error)
	RepoDescribe(ctx context.Context, req *RepoDescribeRequest, accountID string) (*RepoDescribeResponse, error)
	RepoList(ctx context.Context, req *RepoListRequest, accountID string) (*RepoListResponse, error)
	RepoDelete(ctx context.Context, req *RepoDeleteRequest, accountID string) (*RepoDeleteResponse, error)

	PolicyPut(ctx context.Context, req *PolicyPutRequest, accountID string) (*PolicyPutResponse, error)
	PolicyGet(ctx context.Context, req *PolicyGetRequest, accountID string) (*PolicyGetResponse, error)
	PolicyDelete(ctx context.Context, req *PolicyDeleteRequest, accountID string) (*PolicyDeleteResponse, error)

	LifecyclePut(ctx context.Context, req *LifecyclePutRequest, accountID string) (*LifecyclePutResponse, error)
	LifecycleGet(ctx context.Context, req *LifecycleGetRequest, accountID string) (*LifecycleGetResponse, error)
	LifecycleDelete(ctx context.Context, req *LifecycleDeleteRequest, accountID string) (*LifecycleDeleteResponse, error)

	TagPut(ctx context.Context, req *TagPutRequest, accountID string) (*TagPutResponse, error)
	TagGet(ctx context.Context, req *TagGetRequest, accountID string) (*TagGetResponse, error)
	TagList(ctx context.Context, req *TagListRequest, accountID string) (*TagListResponse, error)
	TagDelete(ctx context.Context, req *TagDeleteRequest, accountID string) (*TagDeleteResponse, error)

	ManifestPut(ctx context.Context, req *ManifestPutRequest, accountID string) (*ManifestPutResponse, error)
	ManifestDescribe(ctx context.Context, req *ManifestDescribeRequest, accountID string) (*ManifestDescribeResponse, error)
	ManifestList(ctx context.Context, req *ManifestListRequest, accountID string) (*ManifestListResponse, error)
	ManifestDelete(ctx context.Context, req *ManifestDeleteRequest, accountID string) (*ManifestDeleteResponse, error)

	UploadCreate(ctx context.Context, req *UploadCreateRequest, accountID string) (*UploadCreateResponse, error)
	UploadGet(ctx context.Context, req *UploadGetRequest, accountID string) (*UploadGetResponse, error)
	UploadUpdate(ctx context.Context, req *UploadUpdateRequest, accountID string) (*UploadUpdateResponse, error)
	UploadDelete(ctx context.Context, req *UploadDeleteRequest, accountID string) (*UploadDeleteResponse, error)
}
