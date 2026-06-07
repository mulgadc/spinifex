package handlers_eks

import (
	"encoding/base64"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validToken(url string) string {
	return "k8s-aws-v1." + base64.RawURLEncoding.EncodeToString([]byte(url))
}

const testARN = "arn:aws:iam::111122223333:role/admin"

func okVerify(_ string) (*TokenVerifyResponse, error) {
	return &TokenVerifyResponse{
		AccountID:     "111122223333",
		ARN:           testARN,
		UserID:        "AROAEXAMPLE:session",
		PrincipalType: "AssumedRole",
	}, nil
}

func TestAuthenticate_GrantsWhenEntryExists(t *testing.T) {
	lookup := func(arn string) (*AccessEntryRecord, error) {
		assert.Equal(t, testARN, arn)
		return &AccessEntryRecord{
			KubernetesUsername: testARN,
			KubernetesGroups:   []string{"system:masters"},
		}, nil
	}

	res := Authenticate(validToken("https://sts.amazonaws.com/?Action=GetCallerIdentity"), okVerify, lookup)

	require.True(t, res.Authenticated)
	assert.Equal(t, testARN, res.Username)
	assert.Equal(t, "AROAEXAMPLE:session", res.UID)
	assert.Equal(t, []string{"system:masters"}, res.Groups)
}

func TestAuthenticate_DeniesMalformedToken(t *testing.T) {
	called := false
	verify := func(string) (*TokenVerifyResponse, error) { called = true; return nil, nil }
	lookup := func(string) (*AccessEntryRecord, error) { return nil, nil }

	res := Authenticate("not-a-k8s-aws-token", verify, lookup)

	assert.False(t, res.Authenticated)
	assert.False(t, called, "must not call STS for a token that fails to decode")
}

func TestAuthenticate_DeniesWhenVerifyFails(t *testing.T) {
	verify := func(string) (*TokenVerifyResponse, error) {
		return nil, errors.New("signature mismatch")
	}
	lookup := func(string) (*AccessEntryRecord, error) {
		t.Fatal("lookup must not run when verify fails")
		return nil, nil
	}

	res := Authenticate(validToken("https://sts/?x=1"), verify, lookup)
	assert.False(t, res.Authenticated)
}

func TestAuthenticate_DeniesWhenNoAccessEntry(t *testing.T) {
	lookup := func(string) (*AccessEntryRecord, error) {
		return nil, ErrAccessEntryNotFound
	}

	res := Authenticate(validToken("https://sts/?x=1"), okVerify, lookup)
	assert.False(t, res.Authenticated)
	assert.Empty(t, res.Username)
}

func TestAuthenticate_FallsBackUIDToARN(t *testing.T) {
	verify := func(string) (*TokenVerifyResponse, error) {
		return &TokenVerifyResponse{ARN: testARN}, nil // no UserID
	}
	lookup := func(string) (*AccessEntryRecord, error) {
		return &AccessEntryRecord{KubernetesUsername: testARN, KubernetesGroups: []string{"system:masters"}}, nil
	}

	res := Authenticate(validToken("https://sts/?x=1"), verify, lookup)
	require.True(t, res.Authenticated)
	assert.Equal(t, testARN, res.UID)
}
