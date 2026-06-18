package ecr

import (
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

func (s *NATSMetaStore) PutRepo(accountID string, meta RepoMeta) error {
	_, err := utils.NATSRequest[RepoCreateResponse](s.conn, SubjectRepoCreate, &RepoCreateRequest{Meta: meta}, metaRequestTimeout, accountID)
	return err
}

func (s *NATSMetaStore) GetRepo(accountID, repo string) (RepoMeta, error) {
	resp, err := utils.NATSRequest[RepoDescribeResponse](s.conn, SubjectRepoDescribe, &RepoDescribeRequest{Repo: repo}, metaRequestTimeout, accountID)
	if err != nil {
		return RepoMeta{}, err
	}
	if !resp.Found {
		return RepoMeta{}, ErrNotFound
	}
	return resp.Meta, nil
}

func (s *NATSMetaStore) ListRepos(accountID string) ([]string, error) {
	resp, err := utils.NATSRequest[RepoListResponse](s.conn, SubjectRepoList, &RepoListRequest{}, metaRequestTimeout, accountID)
	if err != nil {
		return nil, err
	}
	return resp.Repos, nil
}

func (s *NATSMetaStore) PutTag(accountID, repo, tag, digest string) error {
	_, err := utils.NATSRequest[TagPutResponse](s.conn, SubjectTagPut, &TagPutRequest{Repo: repo, Tag: tag, Digest: digest}, metaRequestTimeout, accountID)
	return err
}

func (s *NATSMetaStore) GetTag(accountID, repo, tag string) (string, error) {
	resp, err := utils.NATSRequest[TagGetResponse](s.conn, SubjectTagGet, &TagGetRequest{Repo: repo, Tag: tag}, metaRequestTimeout, accountID)
	if err != nil {
		return "", err
	}
	if !resp.Found {
		return "", ErrNotFound
	}
	return resp.Digest, nil
}

func (s *NATSMetaStore) DeleteTag(accountID, repo, tag string) error {
	resp, err := utils.NATSRequest[TagDeleteResponse](s.conn, SubjectTagDelete, &TagDeleteRequest{Repo: repo, Tag: tag}, metaRequestTimeout, accountID)
	if err != nil {
		return err
	}
	if !resp.Found {
		return ErrNotFound
	}
	return nil
}

func (s *NATSMetaStore) ListTags(accountID, repo string) ([]string, error) {
	resp, err := utils.NATSRequest[TagListResponse](s.conn, SubjectTagList, &TagListRequest{Repo: repo}, metaRequestTimeout, accountID)
	if err != nil {
		return nil, err
	}
	return resp.Tags, nil
}

func (s *NATSMetaStore) PutManifestMeta(accountID, repo string, meta ManifestMeta) error {
	_, err := utils.NATSRequest[ManifestPutResponse](s.conn, SubjectManifestPut, &ManifestPutRequest{Repo: repo, Meta: meta}, metaRequestTimeout, accountID)
	return err
}

func (s *NATSMetaStore) GetManifestMeta(accountID, repo, digest string) (ManifestMeta, error) {
	resp, err := utils.NATSRequest[ManifestDescribeResponse](s.conn, SubjectManifestDescribe, &ManifestDescribeRequest{Repo: repo, Digest: digest}, metaRequestTimeout, accountID)
	if err != nil {
		return ManifestMeta{}, err
	}
	if !resp.Found {
		return ManifestMeta{}, ErrNotFound
	}
	return resp.Meta, nil
}

func (s *NATSMetaStore) PutUpload(accountID, uploadID string, state UploadState) (uint64, error) {
	resp, err := utils.NATSRequest[UploadCreateResponse](s.conn, SubjectUploadCreate, &UploadCreateRequest{UploadID: uploadID, State: state}, metaRequestTimeout, accountID)
	if err != nil {
		return 0, err
	}
	return resp.Revision, nil
}

func (s *NATSMetaStore) GetUpload(accountID, uploadID string) (UploadState, uint64, error) {
	resp, err := utils.NATSRequest[UploadGetResponse](s.conn, SubjectUploadGet, &UploadGetRequest{UploadID: uploadID}, metaRequestTimeout, accountID)
	if err != nil {
		return UploadState{}, 0, err
	}
	if !resp.Found {
		return UploadState{}, 0, ErrNotFound
	}
	return resp.State, resp.Revision, nil
}

func (s *NATSMetaStore) UpdateUpload(accountID, uploadID string, state UploadState, rev uint64) (uint64, error) {
	resp, err := utils.NATSRequest[UploadUpdateResponse](s.conn, SubjectUploadUpdate, &UploadUpdateRequest{UploadID: uploadID, State: state, Revision: rev}, metaRequestTimeout, accountID)
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

func (s *NATSMetaStore) DeleteUpload(accountID, uploadID string) error {
	resp, err := utils.NATSRequest[UploadDeleteResponse](s.conn, SubjectUploadDelete, &UploadDeleteRequest{UploadID: uploadID}, metaRequestTimeout, accountID)
	if err != nil {
		return err
	}
	if !resp.Found {
		return ErrNotFound
	}
	return nil
}
