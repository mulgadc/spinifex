package gateway_ecrapi

import (
	"encoding/json"
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/handlers/ecr"
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

// resolveLifecycleRepo parses + validates the request, enforces the registryId
// cross-account guard, and confirms the repository exists.
func resolveLifecycleRepo(nc *nats.Conn, accountID string, body []byte) (lifecyclePolicyRequest, *handlers_ecr.NATSMetaStore, error) {
	var req lifecyclePolicyRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return req, nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}
	if req.RepositoryName == "" || handlers_ecr.ValidateRepoName(req.RepositoryName) != nil {
		return req, nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if req.RegistryID != "" && req.RegistryID != accountID {
		return req, nil, errors.New(awserrors.ErrorAccessDenied)
	}

	store := handlers_ecr.NewNATSMetaStore(nc)
	if _, err := store.GetRepo(accountID, req.RepositoryName); err != nil {
		if errors.Is(err, handlers_ecr.ErrNotFound) {
			return req, nil, errors.New(awserrors.ErrorRepositoryNotFound)
		}
		return req, nil, err
	}
	return req, store, nil
}

// PutLifecyclePolicy validates and stores the lifecycle-policy document for a
// repository. The document is parsed by the evaluation engine; a malformed or
// unsupported rule is rejected with InvalidParameterValue.
func PutLifecyclePolicy(nc *nats.Conn, accountID string, body []byte) (any, error) {
	req, store, err := resolveLifecycleRepo(nc, accountID, body)
	if err != nil {
		return nil, err
	}
	if _, err := handlers_ecr.ParseLifecyclePolicy([]byte(req.LifecyclePolicyText)); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if err := store.PutLifecyclePolicy(accountID, req.RepositoryName, []byte(req.LifecyclePolicyText)); err != nil {
		return nil, err
	}
	return &ecr.PutLifecyclePolicyOutput{
		RegistryId:          aws.String(accountID),
		RepositoryName:      aws.String(req.RepositoryName),
		LifecyclePolicyText: aws.String(req.LifecyclePolicyText),
	}, nil
}

// GetLifecyclePolicy returns the stored lifecycle-policy document, or
// LifecyclePolicyNotFoundException when none is set.
func GetLifecyclePolicy(nc *nats.Conn, accountID string, body []byte) (any, error) {
	req, store, err := resolveLifecycleRepo(nc, accountID, body)
	if err != nil {
		return nil, err
	}
	policy, err := store.GetLifecyclePolicy(accountID, req.RepositoryName)
	if err != nil {
		if errors.Is(err, handlers_ecr.ErrNotFound) {
			return nil, errors.New(awserrors.ErrorLifecyclePolicyNotFound)
		}
		return nil, err
	}
	return &ecr.GetLifecyclePolicyOutput{
		RegistryId:          aws.String(accountID),
		RepositoryName:      aws.String(req.RepositoryName),
		LifecyclePolicyText: aws.String(string(policy)),
	}, nil
}

// DeleteLifecyclePolicy removes and returns the stored lifecycle-policy document,
// or LifecyclePolicyNotFoundException when none is set.
func DeleteLifecyclePolicy(nc *nats.Conn, accountID string, body []byte) (any, error) {
	req, store, err := resolveLifecycleRepo(nc, accountID, body)
	if err != nil {
		return nil, err
	}
	policy, err := store.DeleteLifecyclePolicy(accountID, req.RepositoryName)
	if err != nil {
		if errors.Is(err, handlers_ecr.ErrNotFound) {
			return nil, errors.New(awserrors.ErrorLifecyclePolicyNotFound)
		}
		return nil, err
	}
	return &ecr.DeleteLifecyclePolicyOutput{
		RegistryId:          aws.String(accountID),
		RepositoryName:      aws.String(req.RepositoryName),
		LifecyclePolicyText: aws.String(string(policy)),
	}, nil
}
