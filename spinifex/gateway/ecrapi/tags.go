package gateway_ecrapi

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/handlers/ecr"
	"github.com/nats-io/nats.go"
)

// tagResourceRequest is the TagResource input shape. ecr.Tag carries no
// locationName, so its wire keys are Key/Value (capitalized); reusing the SDK
// type keeps that spelling correct rather than hand-rolling a lowercase one.
type tagResourceRequest struct {
	ResourceArn string     `json:"resourceArn"`
	Tags        []*ecr.Tag `json:"tags"`
}

// untagResourceRequest is the UntagResource input shape.
type untagResourceRequest struct {
	ResourceArn string   `json:"resourceArn"`
	TagKeys     []string `json:"tagKeys"`
}

// listTagsForResourceRequest is the ListTagsForResource input shape.
type listTagsForResourceRequest struct {
	ResourceArn string `json:"resourceArn"`
}

// resolveTaggedRepo parses resourceArn and confirms the repository it names
// exists in the caller account, returning the repo name, its current meta,
// and the NATS-backed MetaStore for a follow-on read-modify-write.
func resolveTaggedRepo(ctx context.Context, nc *nats.Conn, accountID, resourceArn string) (string, handlers_ecr.RepoMeta, *handlers_ecr.NATSMetaStore, error) {
	if resourceArn == "" {
		return "", handlers_ecr.RepoMeta{}, nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	name, err := RepositoryNameFromResourceARN(resourceArn)
	if err != nil {
		return "", handlers_ecr.RepoMeta{}, nil, err
	}
	store := handlers_ecr.NewNATSMetaStore(nc)
	meta, err := store.GetRepo(ctx, accountID, name)
	if err != nil {
		if errors.Is(err, handlers_ecr.ErrNotFound) {
			return "", handlers_ecr.RepoMeta{}, nil, errors.New(awserrors.ErrorRepositoryNotFound)
		}
		return "", handlers_ecr.RepoMeta{}, nil, err
	}
	return name, meta, store, nil
}

// TagResource merges the supplied tags onto the resourceArn-named repository's
// tag map, matching keys overwriting their prior value.
func TagResource(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	var req tagResourceRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}
	if len(req.Tags) == 0 {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	_, meta, store, err := resolveTaggedRepo(ctx, nc, accountID, req.ResourceArn)
	if err != nil {
		return nil, err
	}
	if meta.Tags == nil {
		meta.Tags = make(map[string]string, len(req.Tags))
	}
	for _, t := range req.Tags {
		if t == nil || aws.StringValue(t.Key) == "" {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		meta.Tags[aws.StringValue(t.Key)] = aws.StringValue(t.Value)
	}
	if err := store.PutRepo(ctx, accountID, meta); err != nil {
		return nil, err
	}
	return &ecr.TagResourceOutput{}, nil
}

// UntagResource deletes the named tag keys from the resourceArn-named
// repository's tag map. Removing an absent key is a no-op, matching AWS.
func UntagResource(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	var req untagResourceRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}
	if len(req.TagKeys) == 0 {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	_, meta, store, err := resolveTaggedRepo(ctx, nc, accountID, req.ResourceArn)
	if err != nil {
		return nil, err
	}
	for _, k := range req.TagKeys {
		delete(meta.Tags, k)
	}
	if err := store.PutRepo(ctx, accountID, meta); err != nil {
		return nil, err
	}
	return &ecr.UntagResourceOutput{}, nil
}

// ListTagsForResource returns the tags stored on the resourceArn-named
// repository, sorted by key so repeated reads are stable.
func ListTagsForResource(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	var req listTagsForResourceRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}
	_, meta, _, err := resolveTaggedRepo(ctx, nc, accountID, req.ResourceArn)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(meta.Tags))
	for k := range meta.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tags := make([]*ecr.Tag, 0, len(keys))
	for _, k := range keys {
		tags = append(tags, &ecr.Tag{Key: aws.String(k), Value: aws.String(meta.Tags[k])})
	}
	return &ecr.ListTagsForResourceOutput{Tags: tags}, nil
}
