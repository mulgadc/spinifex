package ecr

import (
	"context"
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

func (s *MetaServiceImpl) RepoCreate(ctx context.Context, req *RepoCreateRequest, accountID string) (*RepoCreateResponse, error) {
	if err := s.store.PutRepo(ctx, accountID, req.Meta); err != nil {
		return nil, err
	}
	return &RepoCreateResponse{}, nil
}

func (s *MetaServiceImpl) RepoDescribe(ctx context.Context, req *RepoDescribeRequest, accountID string) (*RepoDescribeResponse, error) {
	meta, err := s.store.GetRepo(ctx, accountID, req.Repo)
	if errors.Is(err, ErrNotFound) {
		return &RepoDescribeResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &RepoDescribeResponse{Found: true, Meta: meta}, nil
}

func (s *MetaServiceImpl) RepoList(ctx context.Context, _ *RepoListRequest, accountID string) (*RepoListResponse, error) {
	repos, err := s.store.ListRepos(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return &RepoListResponse{Repos: repos}, nil
}

func (s *MetaServiceImpl) RepoDelete(ctx context.Context, req *RepoDeleteRequest, accountID string) (*RepoDeleteResponse, error) {
	err := s.store.DeleteRepo(ctx, accountID, req.Repo)
	if errors.Is(err, ErrNotFound) {
		return &RepoDeleteResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &RepoDeleteResponse{Found: true}, nil
}

func (s *MetaServiceImpl) PolicyPut(ctx context.Context, req *PolicyPutRequest, accountID string) (*PolicyPutResponse, error) {
	if err := s.store.PutRepoPolicy(ctx, accountID, req.Repo, req.PolicyText); err != nil {
		return nil, err
	}
	return &PolicyPutResponse{}, nil
}

func (s *MetaServiceImpl) PolicyGet(ctx context.Context, req *PolicyGetRequest, accountID string) (*PolicyGetResponse, error) {
	policy, err := s.store.GetRepoPolicy(ctx, accountID, req.Repo)
	if errors.Is(err, ErrNotFound) {
		return &PolicyGetResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &PolicyGetResponse{Found: true, PolicyText: policy}, nil
}

func (s *MetaServiceImpl) PolicyDelete(ctx context.Context, req *PolicyDeleteRequest, accountID string) (*PolicyDeleteResponse, error) {
	policy, err := s.store.DeleteRepoPolicy(ctx, accountID, req.Repo)
	if errors.Is(err, ErrNotFound) {
		return &PolicyDeleteResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &PolicyDeleteResponse{Found: true, PolicyText: policy}, nil
}

func (s *MetaServiceImpl) LifecyclePut(ctx context.Context, req *LifecyclePutRequest, accountID string) (*LifecyclePutResponse, error) {
	if err := s.store.PutLifecyclePolicy(ctx, accountID, req.Repo, req.PolicyText); err != nil {
		return nil, err
	}
	return &LifecyclePutResponse{}, nil
}

func (s *MetaServiceImpl) LifecycleGet(ctx context.Context, req *LifecycleGetRequest, accountID string) (*LifecycleGetResponse, error) {
	policy, err := s.store.GetLifecyclePolicy(ctx, accountID, req.Repo)
	if errors.Is(err, ErrNotFound) {
		return &LifecycleGetResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &LifecycleGetResponse{Found: true, PolicyText: policy}, nil
}

func (s *MetaServiceImpl) LifecycleDelete(ctx context.Context, req *LifecycleDeleteRequest, accountID string) (*LifecycleDeleteResponse, error) {
	policy, err := s.store.DeleteLifecyclePolicy(ctx, accountID, req.Repo)
	if errors.Is(err, ErrNotFound) {
		return &LifecycleDeleteResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &LifecycleDeleteResponse{Found: true, PolicyText: policy}, nil
}

func (s *MetaServiceImpl) TagPut(ctx context.Context, req *TagPutRequest, accountID string) (*TagPutResponse, error) {
	if err := s.store.PutTag(ctx, accountID, req.Repo, req.Tag, req.Digest); err != nil {
		return nil, err
	}
	return &TagPutResponse{}, nil
}

func (s *MetaServiceImpl) TagGet(ctx context.Context, req *TagGetRequest, accountID string) (*TagGetResponse, error) {
	digest, err := s.store.GetTag(ctx, accountID, req.Repo, req.Tag)
	if errors.Is(err, ErrNotFound) {
		return &TagGetResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &TagGetResponse{Found: true, Digest: digest}, nil
}

func (s *MetaServiceImpl) TagList(ctx context.Context, req *TagListRequest, accountID string) (*TagListResponse, error) {
	tags, err := s.store.ListTags(ctx, accountID, req.Repo)
	if err != nil {
		return nil, err
	}
	return &TagListResponse{Tags: tags}, nil
}

func (s *MetaServiceImpl) TagDelete(ctx context.Context, req *TagDeleteRequest, accountID string) (*TagDeleteResponse, error) {
	err := s.store.DeleteTag(ctx, accountID, req.Repo, req.Tag)
	if errors.Is(err, ErrNotFound) {
		return &TagDeleteResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &TagDeleteResponse{Found: true}, nil
}

func (s *MetaServiceImpl) ManifestPut(ctx context.Context, req *ManifestPutRequest, accountID string) (*ManifestPutResponse, error) {
	if err := s.store.PutManifestMeta(ctx, accountID, req.Repo, req.Meta); err != nil {
		return nil, err
	}
	return &ManifestPutResponse{}, nil
}

func (s *MetaServiceImpl) ManifestList(ctx context.Context, req *ManifestListRequest, accountID string) (*ManifestListResponse, error) {
	digests, err := s.store.ListManifests(ctx, accountID, req.Repo)
	if err != nil {
		return nil, err
	}
	return &ManifestListResponse{Digests: digests}, nil
}

func (s *MetaServiceImpl) ManifestDelete(ctx context.Context, req *ManifestDeleteRequest, accountID string) (*ManifestDeleteResponse, error) {
	err := s.store.DeleteManifestMeta(ctx, accountID, req.Repo, req.Digest)
	if errors.Is(err, ErrNotFound) {
		return &ManifestDeleteResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &ManifestDeleteResponse{Found: true}, nil
}

func (s *MetaServiceImpl) ManifestDescribe(ctx context.Context, req *ManifestDescribeRequest, accountID string) (*ManifestDescribeResponse, error) {
	meta, err := s.store.GetManifestMeta(ctx, accountID, req.Repo, req.Digest)
	if errors.Is(err, ErrNotFound) {
		return &ManifestDescribeResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &ManifestDescribeResponse{Found: true, Meta: meta}, nil
}

func (s *MetaServiceImpl) UploadCreate(ctx context.Context, req *UploadCreateRequest, accountID string) (*UploadCreateResponse, error) {
	rev, err := s.store.PutUpload(ctx, accountID, req.UploadID, req.State)
	if err != nil {
		return nil, err
	}
	return &UploadCreateResponse{Revision: rev}, nil
}

func (s *MetaServiceImpl) UploadGet(ctx context.Context, req *UploadGetRequest, accountID string) (*UploadGetResponse, error) {
	st, rev, err := s.store.GetUpload(ctx, accountID, req.UploadID)
	if errors.Is(err, ErrNotFound) {
		return &UploadGetResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &UploadGetResponse{Found: true, State: st, Revision: rev}, nil
}

func (s *MetaServiceImpl) UploadUpdate(ctx context.Context, req *UploadUpdateRequest, accountID string) (*UploadUpdateResponse, error) {
	rev, err := s.store.UpdateUpload(ctx, accountID, req.UploadID, req.State, req.Revision)
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

func (s *MetaServiceImpl) UploadDelete(ctx context.Context, req *UploadDeleteRequest, accountID string) (*UploadDeleteResponse, error) {
	err := s.store.DeleteUpload(ctx, accountID, req.UploadID)
	if errors.Is(err, ErrNotFound) {
		return &UploadDeleteResponse{Found: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &UploadDeleteResponse{Found: true}, nil
}
