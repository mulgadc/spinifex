package awsapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	ecrdomain "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// lifecyclePolicyRequest is the shared input shape for the three lifecycle-policy
// CRUD actions. The AWS JSON 1.1 wire keys are camelCase locationNames decoded
// explicitly (the SDK input structs carry no json tags).
type lifecyclePolicyRequest struct {
	RepositoryName      string `json:"repositoryName"`
	RegistryID          string `json:"registryId"`
	LifecyclePolicyText string `json:"lifecyclePolicyText"`
}

// neverEvaluated is the lastEvaluatedAt AWS reports for a lifecycle policy
// that has not been evaluated; the sweeper records no evaluation time.
var neverEvaluated = time.Unix(0, 0).UTC()

// compactPolicyText strips the whitespace between JSON tokens, keeping key
// order, as AWS does; text that is not JSON is returned unchanged.
func compactPolicyText(text []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, text); err != nil {
		return string(text)
	}
	return buf.String()
}

// resolveLifecycleRepo parses + validates the request, enforces the registryId
// cross-account guard, and confirms the repository exists.
func resolveLifecycleRepo(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (lifecyclePolicyRequest, *ecrdomain.NATSMetaStore, error) {
	var req lifecyclePolicyRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return req, nil, MalformedBodyError()
		}
	}
	if err := ValidateRepositoryName(req.RepositoryName); err != nil {
		return req, nil, err
	}
	if req.RegistryID != "" && req.RegistryID != accountID {
		return req, nil, errors.New(awserrors.ErrorAccessDenied)
	}

	store := ecrdomain.NewNATSMetaStore(nc)
	if _, err := store.GetRepo(ctx, accountID, req.RepositoryName); err != nil {
		if errors.Is(err, ecrdomain.ErrNotFound) {
			return req, nil, RepositoryNotFoundError(accountID, req.RepositoryName)
		}
		return req, nil, err
	}
	return req, store, nil
}

// PutLifecyclePolicy validates and stores the lifecycle-policy document for a
// repository. The document is parsed by the evaluation engine; a malformed or
// unsupported rule is rejected with InvalidParameterException.
func PutLifecyclePolicy(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	req, store, err := resolveLifecycleRepo(ctx, nc, accountID, body)
	if err != nil {
		return nil, err
	}
	if _, err := ecrdomain.ParseLifecyclePolicy([]byte(req.LifecyclePolicyText)); err != nil {
		return nil, InvalidLifecyclePolicyError()
	}
	policy := compactPolicyText([]byte(req.LifecyclePolicyText))
	if err := store.PutLifecyclePolicy(ctx, accountID, req.RepositoryName, []byte(policy)); err != nil {
		return nil, err
	}
	return &ecr.PutLifecyclePolicyOutput{
		RegistryId:          aws.String(accountID),
		RepositoryName:      aws.String(req.RepositoryName),
		LifecyclePolicyText: aws.String(policy),
	}, nil
}

// GetLifecyclePolicy returns the stored lifecycle-policy document, or
// LifecyclePolicyNotFoundException when none is set.
func GetLifecyclePolicy(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	req, store, err := resolveLifecycleRepo(ctx, nc, accountID, body)
	if err != nil {
		return nil, err
	}
	policy, err := store.GetLifecyclePolicy(ctx, accountID, req.RepositoryName)
	if err != nil {
		if errors.Is(err, ecrdomain.ErrNotFound) {
			return nil, LifecyclePolicyNotFoundError(accountID, req.RepositoryName)
		}
		return nil, err
	}
	return &ecr.GetLifecyclePolicyOutput{
		RegistryId:          aws.String(accountID),
		RepositoryName:      aws.String(req.RepositoryName),
		LifecyclePolicyText: aws.String(compactPolicyText(policy)),
		LastEvaluatedAt:     aws.Time(neverEvaluated),
	}, nil
}

// DeleteLifecyclePolicy removes and returns the stored lifecycle-policy document,
// or LifecyclePolicyNotFoundException when none is set.
func DeleteLifecyclePolicy(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	req, store, err := resolveLifecycleRepo(ctx, nc, accountID, body)
	if err != nil {
		return nil, err
	}
	policy, err := store.DeleteLifecyclePolicy(ctx, accountID, req.RepositoryName)
	if err != nil {
		if errors.Is(err, ecrdomain.ErrNotFound) {
			return nil, LifecyclePolicyNotFoundError(accountID, req.RepositoryName)
		}
		return nil, err
	}
	return &ecr.DeleteLifecyclePolicyOutput{
		RegistryId:          aws.String(accountID),
		RepositoryName:      aws.String(req.RepositoryName),
		LifecyclePolicyText: aws.String(compactPolicyText(policy)),
		LastEvaluatedAt:     aws.Time(neverEvaluated),
	}, nil
}
