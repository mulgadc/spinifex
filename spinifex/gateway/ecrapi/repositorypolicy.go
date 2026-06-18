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

// repoPolicyRequest is the shared input shape for the three repository-policy
// actions. The AWS JSON 1.1 wire keys are camelCase locationNames; the SDK
// input structs carry no json tags, so the fields are decoded explicitly.
type repoPolicyRequest struct {
	RepositoryName string `json:"repositoryName"`
	RegistryID     string `json:"registryId"`
	PolicyText     string `json:"policyText"`
}

// resolvePolicyRepo parses and validates the request, enforces the registryId
// cross-account guard, and confirms the repository exists. It returns the parsed
// request and the NATS-backed MetaStore for the follow-on policy operation.
func resolvePolicyRepo(nc *nats.Conn, accountID string, body []byte) (repoPolicyRequest, *handlers_ecr.NATSMetaStore, error) {
	var req repoPolicyRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return req, nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}
	if req.RepositoryName == "" || handlers_ecr.ValidateRepoName(req.RepositoryName) != nil {
		return req, nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	// Cross-account registry access is the Q8 parity gap pending registry-policy
	// v2; a registryId naming a different account is denied.
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

// SetRepositoryPolicy stores the JSON IAM policy document for a repository. The
// policy is passthrough metadata in v1: it is persisted and returned but not
// evaluated for cross-account access (Q8; registry-policy evaluator is v2).
func SetRepositoryPolicy(nc *nats.Conn, accountID string, body []byte) (any, error) {
	req, store, err := resolvePolicyRepo(nc, accountID, body)
	if err != nil {
		return nil, err
	}
	if req.PolicyText == "" || !json.Valid([]byte(req.PolicyText)) {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if err := store.PutRepoPolicy(accountID, req.RepositoryName, []byte(req.PolicyText)); err != nil {
		return nil, err
	}
	return &ecr.SetRepositoryPolicyOutput{
		RegistryId:     aws.String(accountID),
		RepositoryName: aws.String(req.RepositoryName),
		PolicyText:     aws.String(req.PolicyText),
	}, nil
}

// GetRepositoryPolicy returns the stored policy document, or
// RepositoryPolicyNotFoundException when none is set.
func GetRepositoryPolicy(nc *nats.Conn, accountID string, body []byte) (any, error) {
	req, store, err := resolvePolicyRepo(nc, accountID, body)
	if err != nil {
		return nil, err
	}
	policy, err := store.GetRepoPolicy(accountID, req.RepositoryName)
	if err != nil {
		if errors.Is(err, handlers_ecr.ErrNotFound) {
			return nil, errors.New(awserrors.ErrorRepositoryPolicyNotFound)
		}
		return nil, err
	}
	return &ecr.GetRepositoryPolicyOutput{
		RegistryId:     aws.String(accountID),
		RepositoryName: aws.String(req.RepositoryName),
		PolicyText:     aws.String(string(policy)),
	}, nil
}

// DeleteRepositoryPolicy removes and returns the stored policy document, or
// RepositoryPolicyNotFoundException when none is set.
func DeleteRepositoryPolicy(nc *nats.Conn, accountID string, body []byte) (any, error) {
	req, store, err := resolvePolicyRepo(nc, accountID, body)
	if err != nil {
		return nil, err
	}
	policy, err := store.DeleteRepoPolicy(accountID, req.RepositoryName)
	if err != nil {
		if errors.Is(err, handlers_ecr.ErrNotFound) {
			return nil, errors.New(awserrors.ErrorRepositoryPolicyNotFound)
		}
		return nil, err
	}
	return &ecr.DeleteRepositoryPolicyOutput{
		RegistryId:     aws.String(accountID),
		RepositoryName: aws.String(req.RepositoryName),
		PolicyText:     aws.String(string(policy)),
	}, nil
}
