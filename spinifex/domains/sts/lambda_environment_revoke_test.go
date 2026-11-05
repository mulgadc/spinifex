//test:in-package — fault injection replaces STSServiceImpl.sessions
package sts

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lambdaScope(environmentID string) LambdaEnvironmentSessionScope {
	return LambdaEnvironmentSessionScope{
		AccountID:     testCallerAccountID,
		FunctionID:    testLambdaFunctionID,
		FunctionARN:   testLambdaFunctionARN,
		EnvironmentID: environmentID,
	}
}

func TestRevokeLambdaEnvironmentSession_DeletesOnlyAMatchingRecord(t *testing.T) {
	svc, _ := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, testLambdaRoleName, lambdaTrustPolicyDoc)
	akid := issueLambdaSession(t, svc, *role.Arn, testLambdaEnvID, 3600)

	wrong := map[string]func(*LambdaEnvironmentSessionScope){
		"account":     func(s *LambdaEnvironmentSessionScope) { s.AccountID = "000000000002" },
		"function ID": func(s *LambdaEnvironmentSessionScope) { s.FunctionID = "f1" },
		"function ARN": func(s *LambdaEnvironmentSessionScope) {
			s.FunctionARN = "arn:aws:lambda:ap-southeast-2:" + testCallerAccountID + ":function:other"
		},
		"environment ID": func(s *LambdaEnvironmentSessionScope) { s.EnvironmentID = "env-0002" },
	}
	for name, mutate := range wrong {
		t.Run("wrong "+name, func(t *testing.T) {
			scope := lambdaScope(testLambdaEnvID)
			mutate(&scope)
			require.ErrorIs(t, svc.RevokeLambdaEnvironmentSession(t.Context(), akid, scope), ErrLambdaSessionScopeMismatch)
			assertSessionsKept(t, svc, akid)
		})
	}

	require.NoError(t, svc.RevokeLambdaEnvironmentSession(t.Context(), akid, lambdaScope(testLambdaEnvID)))
	assertSessionsGone(t, svc, akid)
	require.ErrorIs(t, svc.RevokeLambdaEnvironmentSession(t.Context(), akid, lambdaScope(testLambdaEnvID)), ErrLambdaSessionNotFound)
	require.ErrorIs(t, svc.RevokeLambdaEnvironmentSession(t.Context(), "", lambdaScope(testLambdaEnvID)), ErrLambdaSessionNotFound)
}

// A non-Lambda session carries empty labels, so an empty scope must not match it.
func TestRevokeLambdaEnvironmentSession_LeavesOtherSessionKinds(t *testing.T) {
	svc, _ := newTestSetup(t)
	seedUser(t, svc, testCallerAccountID, testCallerUserName)
	user := mintUserSession(t, svc, testCallerAccountID, testCallerUserName).AccessKeyID

	err := svc.RevokeLambdaEnvironmentSession(t.Context(), user, LambdaEnvironmentSessionScope{AccountID: testCallerAccountID})
	require.ErrorIs(t, err, ErrLambdaSessionScopeMismatch)
	assertSessionsKept(t, svc, user)
}

// replacingKV rewrites the record right after the revoke reads it.
type replacingKV struct {
	jetstream.KeyValue
	replaced bool
}

func (r *replacingKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	entry, err := r.KeyValue.Get(ctx, key)
	if err == nil && !r.replaced {
		r.replaced = true
		if _, perr := r.KeyValue.Put(ctx, key, entry.Value()); perr != nil {
			return nil, perr
		}
	}
	return entry, err
}

func TestRevokeLambdaEnvironmentSession_KeepsARecordReplacedAfterTheRead(t *testing.T) {
	svc, _ := newTestSetup(t)
	role := createRoleInAccount(t, svc, testCallerAccountID, testLambdaRoleName, lambdaTrustPolicyDoc)
	akid := issueLambdaSession(t, svc, *role.Arn, testLambdaEnvID, 3600)

	original := svc.sessions
	swap := &replacingKV{KeyValue: sessionsKV(t, svc)}
	svc.sessions = &kvstore.Store[SessionCredential]{Bucket: kvstore.NewOpenBucket(nil, swap, sessionCredentialsConfig())}

	err := svc.RevokeLambdaEnvironmentSession(t.Context(), akid, lambdaScope(testLambdaEnvID))
	require.ErrorIs(t, err, ErrLambdaSessionRevokeFailed)
	assert.True(t, swap.replaced)
	assert.NotContains(t, err.Error(), akid)

	svc.sessions = original
	assertSessionsKept(t, svc, akid)
}

// keyLeakingKV fails Get or Delete with an error naming the key, as a backend may.
type keyLeakingKV struct {
	jetstream.KeyValue
	failGet, failDelete bool
}

func (k *keyLeakingKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if k.failGet {
		return nil, errors.New("backend: get " + key + " failed")
	}
	return k.KeyValue.Get(ctx, key)
}

func (k *keyLeakingKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if k.failDelete {
		return errors.New("backend: delete " + key + " failed")
	}
	return k.KeyValue.Delete(ctx, key, opts...)
}

func TestRevokeLambdaEnvironmentSession_BackendErrorsDoNotCarryTheKey(t *testing.T) {
	for name, kv := range map[string]*keyLeakingKV{
		"get":    {failGet: true},
		"delete": {failDelete: true},
	} {
		t.Run(name, func(t *testing.T) {
			svc, _ := newTestSetup(t)
			role := createRoleInAccount(t, svc, testCallerAccountID, testLambdaRoleName, lambdaTrustPolicyDoc)
			akid := issueLambdaSession(t, svc, *role.Arn, testLambdaEnvID, 3600)
			var logs bytes.Buffer
			captureSlog(t, &logs)

			original := svc.sessions
			kv.KeyValue = sessionsKV(t, svc)
			svc.sessions = &kvstore.Store[SessionCredential]{Bucket: kvstore.NewOpenBucket(nil, kv, sessionCredentialsConfig())}

			err := svc.RevokeLambdaEnvironmentSession(t.Context(), akid, lambdaScope(testLambdaEnvID))
			require.ErrorIs(t, err, ErrLambdaSessionRevokeFailed)
			assert.NotContains(t, err.Error(), akid)
			assert.NotContains(t, logs.String(), akid)

			svc.sessions = original
			assertSessionsKept(t, svc, akid)
		})
	}
}
