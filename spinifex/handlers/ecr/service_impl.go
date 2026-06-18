package ecr

import (
	"errors"

	"github.com/nats-io/nats.go"
)

// MetaServiceImpl is the daemon-side MetaService. It owns the backing MetaStore
// (JetStream KV in production) and translates ErrNotFound/ErrConflict into the
// response flags so the gateway client can reconstruct them across NATS.
type MetaServiceImpl struct {
	store MetaStore
}

var _ MetaService = (*MetaServiceImpl)(nil)

// NewMetaServiceImpl builds a MetaService over an explicit MetaStore (used by
// daemon-side tests with a MemoryMetaStore).
func NewMetaServiceImpl(store MetaStore) *MetaServiceImpl {
	return &MetaServiceImpl{store: store}
}

// NewKVMetaService builds a MetaService backed by per-account JetStream KV.
func NewKVMetaService(js nats.JetStreamContext) *MetaServiceImpl {
	return &MetaServiceImpl{store: NewKVMetaStore(js)}
}

func (s *MetaServiceImpl) RepoCreate(req *RepoCreateRequest, accountID string) (*RepoCreateResponse, error) {
	if err := s.store.PutRepo(accountID, req.Meta); err != nil {
		return nil, err
	}
	return &RepoCreateResponse{}, nil
}

func (s *MetaServiceImpl) RepoDescribe(req *RepoDescribeRequest, accountID string) (*RepoDescribeResponse, error) {
	meta, err := s.store.GetRepo(accountID, req.Repo)
	if errors.Is(err, ErrNotFound) {
		return &RepoDescribeResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &RepoDescribeResponse{Found: true, Meta: meta}, nil
}

func (s *MetaServiceImpl) RepoList(_ *RepoListRequest, accountID string) (*RepoListResponse, error) {
	repos, err := s.store.ListRepos(accountID)
	if err != nil {
		return nil, err
	}
	return &RepoListResponse{Repos: repos}, nil
}

func (s *MetaServiceImpl) PolicyPut(req *PolicyPutRequest, accountID string) (*PolicyPutResponse, error) {
	if err := s.store.PutRepoPolicy(accountID, req.Repo, req.PolicyText); err != nil {
		return nil, err
	}
	return &PolicyPutResponse{}, nil
}

func (s *MetaServiceImpl) PolicyGet(req *PolicyGetRequest, accountID string) (*PolicyGetResponse, error) {
	policy, err := s.store.GetRepoPolicy(accountID, req.Repo)
	if errors.Is(err, ErrNotFound) {
		return &PolicyGetResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &PolicyGetResponse{Found: true, PolicyText: policy}, nil
}

func (s *MetaServiceImpl) PolicyDelete(req *PolicyDeleteRequest, accountID string) (*PolicyDeleteResponse, error) {
	policy, err := s.store.DeleteRepoPolicy(accountID, req.Repo)
	if errors.Is(err, ErrNotFound) {
		return &PolicyDeleteResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &PolicyDeleteResponse{Found: true, PolicyText: policy}, nil
}

func (s *MetaServiceImpl) TagPut(req *TagPutRequest, accountID string) (*TagPutResponse, error) {
	if err := s.store.PutTag(accountID, req.Repo, req.Tag, req.Digest); err != nil {
		return nil, err
	}
	return &TagPutResponse{}, nil
}

func (s *MetaServiceImpl) TagGet(req *TagGetRequest, accountID string) (*TagGetResponse, error) {
	digest, err := s.store.GetTag(accountID, req.Repo, req.Tag)
	if errors.Is(err, ErrNotFound) {
		return &TagGetResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &TagGetResponse{Found: true, Digest: digest}, nil
}

func (s *MetaServiceImpl) TagList(req *TagListRequest, accountID string) (*TagListResponse, error) {
	tags, err := s.store.ListTags(accountID, req.Repo)
	if err != nil {
		return nil, err
	}
	return &TagListResponse{Tags: tags}, nil
}

func (s *MetaServiceImpl) TagDelete(req *TagDeleteRequest, accountID string) (*TagDeleteResponse, error) {
	err := s.store.DeleteTag(accountID, req.Repo, req.Tag)
	if errors.Is(err, ErrNotFound) {
		return &TagDeleteResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &TagDeleteResponse{Found: true}, nil
}

func (s *MetaServiceImpl) ManifestPut(req *ManifestPutRequest, accountID string) (*ManifestPutResponse, error) {
	if err := s.store.PutManifestMeta(accountID, req.Repo, req.Meta); err != nil {
		return nil, err
	}
	return &ManifestPutResponse{}, nil
}

func (s *MetaServiceImpl) ManifestDescribe(req *ManifestDescribeRequest, accountID string) (*ManifestDescribeResponse, error) {
	meta, err := s.store.GetManifestMeta(accountID, req.Repo, req.Digest)
	if errors.Is(err, ErrNotFound) {
		return &ManifestDescribeResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &ManifestDescribeResponse{Found: true, Meta: meta}, nil
}

func (s *MetaServiceImpl) UploadCreate(req *UploadCreateRequest, accountID string) (*UploadCreateResponse, error) {
	rev, err := s.store.PutUpload(accountID, req.UploadID, req.State)
	if err != nil {
		return nil, err
	}
	return &UploadCreateResponse{Revision: rev}, nil
}

func (s *MetaServiceImpl) UploadGet(req *UploadGetRequest, accountID string) (*UploadGetResponse, error) {
	st, rev, err := s.store.GetUpload(accountID, req.UploadID)
	if errors.Is(err, ErrNotFound) {
		return &UploadGetResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &UploadGetResponse{Found: true, State: st, Revision: rev}, nil
}

func (s *MetaServiceImpl) UploadUpdate(req *UploadUpdateRequest, accountID string) (*UploadUpdateResponse, error) {
	rev, err := s.store.UpdateUpload(accountID, req.UploadID, req.State, req.Revision)
	if errors.Is(err, ErrNotFound) {
		return &UploadUpdateResponse{Found: false}, nil
	}
	if errors.Is(err, ErrConflict) {
		return &UploadUpdateResponse{Found: true, Conflict: true}, nil
	}
	if err != nil {
		return nil, err
	}
	return &UploadUpdateResponse{Found: true, Revision: rev}, nil
}

func (s *MetaServiceImpl) UploadDelete(req *UploadDeleteRequest, accountID string) (*UploadDeleteResponse, error) {
	err := s.store.DeleteUpload(accountID, req.UploadID)
	if errors.Is(err, ErrNotFound) {
		return &UploadDeleteResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &UploadDeleteResponse{Found: true}, nil
}
