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

type RepoCreateRequest struct {
	Meta RepoMeta `json:"meta"`
}

type RepoCreateResponse struct{}

type RepoDescribeRequest struct {
	Repo string `json:"repo"`
}

type RepoDescribeResponse struct {
	Found bool     `json:"found"`
	Meta  RepoMeta `json:"meta"`
}

type RepoListRequest struct{}

type RepoListResponse struct {
	Repos []string `json:"repos"`
}

type RepoDeleteRequest struct {
	Repo string `json:"repo"`
}

type RepoDeleteResponse struct {
	Found bool `json:"found"`
}

type PolicyPutRequest struct {
	Repo       string `json:"repo"`
	PolicyText []byte `json:"policyText"`
}

type PolicyPutResponse struct{}

type PolicyGetRequest struct {
	Repo string `json:"repo"`
}

type PolicyGetResponse struct {
	Found      bool   `json:"found"`
	PolicyText []byte `json:"policyText"`
}

type PolicyDeleteRequest struct {
	Repo string `json:"repo"`
}

type PolicyDeleteResponse struct {
	Found      bool   `json:"found"`
	PolicyText []byte `json:"policyText"`
}

type LifecyclePutRequest struct {
	Repo       string `json:"repo"`
	PolicyText []byte `json:"policyText"`
}

type LifecyclePutResponse struct{}

type LifecycleGetRequest struct {
	Repo string `json:"repo"`
}

type LifecycleGetResponse struct {
	Found      bool   `json:"found"`
	PolicyText []byte `json:"policyText"`
}

type LifecycleDeleteRequest struct {
	Repo string `json:"repo"`
}

type LifecycleDeleteResponse struct {
	Found      bool   `json:"found"`
	PolicyText []byte `json:"policyText"`
}

type TagPutRequest struct {
	Repo   string `json:"repo"`
	Tag    string `json:"tag"`
	Digest string `json:"digest"`
}

type TagPutResponse struct{}

type TagGetRequest struct {
	Repo string `json:"repo"`
	Tag  string `json:"tag"`
}

type TagGetResponse struct {
	Found  bool   `json:"found"`
	Digest string `json:"digest"`
}

type TagListRequest struct {
	Repo string `json:"repo"`
}

type TagListResponse struct {
	Tags []string `json:"tags"`
}

type TagDeleteRequest struct {
	Repo string `json:"repo"`
	Tag  string `json:"tag"`
}

type TagDeleteResponse struct {
	Found bool `json:"found"`
}

type ManifestPutRequest struct {
	Repo string       `json:"repo"`
	Meta ManifestMeta `json:"meta"`
}

type ManifestPutResponse struct{}

type ManifestDescribeRequest struct {
	Repo   string `json:"repo"`
	Digest string `json:"digest"`
}

type ManifestDescribeResponse struct {
	Found bool         `json:"found"`
	Meta  ManifestMeta `json:"meta"`
}

type ManifestListRequest struct {
	Repo string `json:"repo"`
}

type ManifestListResponse struct {
	Digests []string `json:"digests"`
}

type ManifestDeleteRequest struct {
	Repo   string `json:"repo"`
	Digest string `json:"digest"`
}

type ManifestDeleteResponse struct {
	Found bool `json:"found"`
}

type UploadCreateRequest struct {
	UploadID string      `json:"uploadID"`
	State    UploadState `json:"state"`
}

type UploadCreateResponse struct {
	Revision uint64 `json:"revision"`
}

type UploadGetRequest struct {
	UploadID string `json:"uploadID"`
}

type UploadGetResponse struct {
	Found    bool        `json:"found"`
	State    UploadState `json:"state"`
	Revision uint64      `json:"revision"`
}

type UploadUpdateRequest struct {
	UploadID string      `json:"uploadID"`
	State    UploadState `json:"state"`
	Revision uint64      `json:"revision"`
}

type UploadUpdateResponse struct {
	Found    bool   `json:"found"`
	Conflict bool   `json:"conflict"`
	Revision uint64 `json:"revision"`
}

type UploadDeleteRequest struct {
	UploadID string `json:"uploadID"`
}

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
