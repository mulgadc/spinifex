package ecr

import (
	"context"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const metaRequestTimeout = 30 * time.Second

// NATSMetaStore is the gateway-side MetaStore. It forwards every metadata
// operation to the daemon over NATS request/reply; the daemon owns the KV. The
// Found/Conflict response flags are mapped back to ErrNotFound/ErrConflict so
// callers see the same MetaStore contract as the in-process stores.
type NATSMetaStore struct {
	conn *nats.Conn
}

var _ MetaStore = (*NATSMetaStore)(nil)

// NewNATSMetaStore builds a NATS-backed MetaStore client.
func NewNATSMetaStore(conn *nats.Conn) *NATSMetaStore {
	return &NATSMetaStore{conn: conn}
}

func (s *NATSMetaStore) PutRepo(ctx context.Context, accountID string, meta RepoMeta) error {
	_, err := utils.NatsRequest[RepoCreateResponse](ctx, s.conn, SubjectRepoCreate, &RepoCreateRequest{Meta: meta}, metaRequestTimeout, accountID)
	return err
}

func (s *NATSMetaStore) GetRepo(ctx context.Context, accountID, repo string) (RepoMeta, error) {
	resp, err := utils.NatsRequest[RepoDescribeResponse](ctx, s.conn, SubjectRepoDescribe, &RepoDescribeRequest{Repo: repo}, metaRequestTimeout, accountID)
	if err != nil {
		return RepoMeta{}, err
	}
	if !resp.Found {
		return RepoMeta{}, ErrNotFound
	}
	return resp.Meta, nil
}

func (s *NATSMetaStore) ListRepos(ctx context.Context, accountID string) ([]string, error) {
	resp, err := utils.NatsRequest[RepoListResponse](ctx, s.conn, SubjectRepoList, &RepoListRequest{}, metaRequestTimeout, accountID)
	if err != nil {
		return nil, err
	}
	return resp.Repos, nil
}

func (s *NATSMetaStore) DeleteRepo(ctx context.Context, accountID, repo string) error {
	resp, err := utils.NatsRequest[RepoDeleteResponse](ctx, s.conn, SubjectRepoDelete, &RepoDeleteRequest{Repo: repo}, metaRequestTimeout, accountID)
	if err != nil {
		return err
	}
	if !resp.Found {
		return ErrNotFound
	}
	return nil
}

func (s *NATSMetaStore) ListManifests(ctx context.Context, accountID, repo string) ([]string, error) {
	resp, err := utils.NatsRequest[ManifestListResponse](ctx, s.conn, SubjectManifestList, &ManifestListRequest{Repo: repo}, metaRequestTimeout, accountID)
	if err != nil {
		return nil, err
	}
	return resp.Digests, nil
}

func (s *NATSMetaStore) PutRepoPolicy(ctx context.Context, accountID, repo string, policyText []byte) error {
	_, err := utils.NatsRequest[PolicyPutResponse](ctx, s.conn, SubjectPolicyPut, &PolicyPutRequest{Repo: repo, PolicyText: policyText}, metaRequestTimeout, accountID)
	return err
}

func (s *NATSMetaStore) GetRepoPolicy(ctx context.Context, accountID, repo string) ([]byte, error) {
	resp, err := utils.NatsRequest[PolicyGetResponse](ctx, s.conn, SubjectPolicyGet, &PolicyGetRequest{Repo: repo}, metaRequestTimeout, accountID)
	if err != nil {
		return nil, err
	}
	if !resp.Found {
		return nil, ErrNotFound
	}
	return resp.PolicyText, nil
}

func (s *NATSMetaStore) DeleteRepoPolicy(ctx context.Context, accountID, repo string) ([]byte, error) {
	resp, err := utils.NatsRequest[PolicyDeleteResponse](ctx, s.conn, SubjectPolicyDelete, &PolicyDeleteRequest{Repo: repo}, metaRequestTimeout, accountID)
	if err != nil {
		return nil, err
	}
	if !resp.Found {
		return nil, ErrNotFound
	}
	return resp.PolicyText, nil
}

func (s *NATSMetaStore) PutLifecyclePolicy(ctx context.Context, accountID, repo string, policyText []byte) error {
	_, err := utils.NatsRequest[LifecyclePutResponse](ctx, s.conn, SubjectLifecyclePut, &LifecyclePutRequest{Repo: repo, PolicyText: policyText}, metaRequestTimeout, accountID)
	return err
}

func (s *NATSMetaStore) GetLifecyclePolicy(ctx context.Context, accountID, repo string) ([]byte, error) {
	resp, err := utils.NatsRequest[LifecycleGetResponse](ctx, s.conn, SubjectLifecycleGet, &LifecycleGetRequest{Repo: repo}, metaRequestTimeout, accountID)
	if err != nil {
		return nil, err
	}
	if !resp.Found {
		return nil, ErrNotFound
	}
	return resp.PolicyText, nil
}

func (s *NATSMetaStore) DeleteLifecyclePolicy(ctx context.Context, accountID, repo string) ([]byte, error) {
	resp, err := utils.NatsRequest[LifecycleDeleteResponse](ctx, s.conn, SubjectLifecycleDelete, &LifecycleDeleteRequest{Repo: repo}, metaRequestTimeout, accountID)
	if err != nil {
		return nil, err
	}
	if !resp.Found {
		return nil, ErrNotFound
	}
	return resp.PolicyText, nil
}

func (s *NATSMetaStore) PutTag(ctx context.Context, accountID, repo, tag, digest string) error {
	_, err := utils.NatsRequest[TagPutResponse](ctx, s.conn, SubjectTagPut, &TagPutRequest{Repo: repo, Tag: tag, Digest: digest}, metaRequestTimeout, accountID)
	return err
}

func (s *NATSMetaStore) GetTag(ctx context.Context, accountID, repo, tag string) (string, error) {
	resp, err := utils.NatsRequest[TagGetResponse](ctx, s.conn, SubjectTagGet, &TagGetRequest{Repo: repo, Tag: tag}, metaRequestTimeout, accountID)
	if err != nil {
		return "", err
	}
	if !resp.Found {
		return "", ErrNotFound
	}
	return resp.Digest, nil
}

func (s *NATSMetaStore) DeleteTag(ctx context.Context, accountID, repo, tag string) error {
	resp, err := utils.NatsRequest[TagDeleteResponse](ctx, s.conn, SubjectTagDelete, &TagDeleteRequest{Repo: repo, Tag: tag}, metaRequestTimeout, accountID)
	if err != nil {
		return err
	}
	if !resp.Found {
		return ErrNotFound
	}
	return nil
}

func (s *NATSMetaStore) ListTags(ctx context.Context, accountID, repo string) ([]string, error) {
	resp, err := utils.NatsRequest[TagListResponse](ctx, s.conn, SubjectTagList, &TagListRequest{Repo: repo}, metaRequestTimeout, accountID)
	if err != nil {
		return nil, err
	}
	return resp.Tags, nil
}

func (s *NATSMetaStore) PutManifestMeta(ctx context.Context, accountID, repo string, meta ManifestMeta) error {
	_, err := utils.NatsRequest[ManifestPutResponse](ctx, s.conn, SubjectManifestPut, &ManifestPutRequest{Repo: repo, Meta: meta}, metaRequestTimeout, accountID)
	return err
}

func (s *NATSMetaStore) GetManifestMeta(ctx context.Context, accountID, repo, digest string) (ManifestMeta, error) {
	resp, err := utils.NatsRequest[ManifestDescribeResponse](ctx, s.conn, SubjectManifestDescribe, &ManifestDescribeRequest{Repo: repo, Digest: digest}, metaRequestTimeout, accountID)
	if err != nil {
		return ManifestMeta{}, err
	}
	if !resp.Found {
		return ManifestMeta{}, ErrNotFound
	}
	return resp.Meta, nil
}

func (s *NATSMetaStore) DeleteManifestMeta(ctx context.Context, accountID, repo, digest string) error {
	resp, err := utils.NatsRequest[ManifestDeleteResponse](ctx, s.conn, SubjectManifestDelete, &ManifestDeleteRequest{Repo: repo, Digest: digest}, metaRequestTimeout, accountID)
	if err != nil {
		return err
	}
	if !resp.Found {
		return ErrNotFound
	}
	return nil
}

func (s *NATSMetaStore) PutUpload(ctx context.Context, accountID, uploadID string, state UploadState) (uint64, error) {
	resp, err := utils.NatsRequest[UploadCreateResponse](ctx, s.conn, SubjectUploadCreate, &UploadCreateRequest{UploadID: uploadID, State: state}, metaRequestTimeout, accountID)
	if err != nil {
		return 0, err
	}
	return resp.Revision, nil
}

func (s *NATSMetaStore) GetUpload(ctx context.Context, accountID, uploadID string) (UploadState, uint64, error) {
	resp, err := utils.NatsRequest[UploadGetResponse](ctx, s.conn, SubjectUploadGet, &UploadGetRequest{UploadID: uploadID}, metaRequestTimeout, accountID)
	if err != nil {
		return UploadState{}, 0, err
	}
	if !resp.Found {
		return UploadState{}, 0, ErrNotFound
	}
	return resp.State, resp.Revision, nil
}

func (s *NATSMetaStore) UpdateUpload(ctx context.Context, accountID, uploadID string, state UploadState, rev uint64) (uint64, error) {
	resp, err := utils.NatsRequest[UploadUpdateResponse](ctx, s.conn, SubjectUploadUpdate, &UploadUpdateRequest{UploadID: uploadID, State: state, Revision: rev}, metaRequestTimeout, accountID)
	if err != nil {
		return 0, err
	}
	if !resp.Found {
		return 0, ErrNotFound
	}
	if resp.Conflict {
		return 0, ErrConflict
	}
	return resp.Revision, nil
}

func (s *NATSMetaStore) DeleteUpload(ctx context.Context, accountID, uploadID string) error {
	resp, err := utils.NatsRequest[UploadDeleteResponse](ctx, s.conn, SubjectUploadDelete, &UploadDeleteRequest{UploadID: uploadID}, metaRequestTimeout, accountID)
	if err != nil {
		return err
	}
	if !resp.Found {
		return ErrNotFound
	}
	return nil
}
