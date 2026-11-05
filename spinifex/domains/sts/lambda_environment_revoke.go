package sts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

// LambdaEnvironmentSessionScope names what a Lambda environment session was issued
// for; a revoke deletes the record only when every field matches it.
type LambdaEnvironmentSessionScope struct {
	AccountID     string
	FunctionID    string
	FunctionARN   string
	EnvironmentID string
}

var (
	// ErrLambdaSessionNotFound means no record exists for the access key.
	ErrLambdaSessionNotFound = errors.New("lambda environment session not found")
	// ErrLambdaSessionScopeMismatch means the record was not issued for the scope.
	ErrLambdaSessionScopeMismatch = errors.New("lambda environment session scope mismatch")
	// ErrLambdaSessionRevokeFailed means the record could not be read, decoded or
	// deleted; the backend's error is dropped because it can name the key.
	ErrLambdaSessionRevokeFailed = errors.New("lambda environment session revoke failed")
)

// RevokeLambdaEnvironmentSession deletes one environment session record, only if
// it was issued for scope and only at the revision read, so a record replaced in
// between is kept. Errors never carry the access key.
func (s *STSServiceImpl) RevokeLambdaEnvironmentSession(ctx context.Context, accessKeyID string, scope LambdaEnvironmentSessionScope) error {
	if accessKeyID == "" {
		return ErrLambdaSessionNotFound
	}
	bucket, err := s.sessions.KV(ctx)
	if err != nil {
		return fmt.Errorf("open session credential bucket: %w", err)
	}
	entry, err := bucket.Get(ctx, accessKeyID)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return ErrLambdaSessionNotFound
		}
		return fmt.Errorf("%w: read", ErrLambdaSessionRevokeFailed)
	}

	var cred SessionCredential
	if err := json.Unmarshal(entry.Value(), &cred); err != nil {
		return fmt.Errorf("%w: decode", ErrLambdaSessionRevokeFailed)
	}
	if cred.AccountID != scope.AccountID || cred.LambdaFunctionID != scope.FunctionID ||
		cred.LambdaFunctionARN != scope.FunctionARN || cred.LambdaEnvironmentID != scope.EnvironmentID ||
		scope.EnvironmentID == "" {
		return ErrLambdaSessionScopeMismatch
	}

	if err := bucket.Delete(ctx, accessKeyID, jetstream.LastRevision(entry.Revision())); err != nil {
		return fmt.Errorf("%w: delete at revision %d", ErrLambdaSessionRevokeFailed, entry.Revision())
	}
	return nil
}
