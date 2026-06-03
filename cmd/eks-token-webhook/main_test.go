package main

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validToken(url string) string {
	return "k8s-aws-v1." + base64.RawURLEncoding.EncodeToString([]byte(url))
}

const testARN = "arn:aws:iam::111122223333:role/admin"

func okVerify(_ string) (*handlers_eks.TokenVerifyResponse, error) {
	return &handlers_eks.TokenVerifyResponse{
		AccountID:     "111122223333",
		ARN:           testARN,
		UserID:        "AROAEXAMPLE:session",
		PrincipalType: "AssumedRole",
	}, nil
}

func TestAuthenticate_GrantsWhenEntryExists(t *testing.T) {
	lookup := func(arn string) (*handlers_eks.AccessEntryRecord, error) {
		assert.Equal(t, testARN, arn)
		return &handlers_eks.AccessEntryRecord{
			KubernetesUsername: testARN,
			KubernetesGroups:   []string{"system:masters"},
		}, nil
	}

	st := authenticate(validToken("https://sts.amazonaws.com/?Action=GetCallerIdentity"), okVerify, lookup)

	require.True(t, st.Authenticated)
	assert.Equal(t, testARN, st.User.Username)
	assert.Equal(t, "AROAEXAMPLE:session", st.User.UID)
	assert.Equal(t, []string{"system:masters"}, st.User.Groups)
}

func TestAuthenticate_DeniesMalformedToken(t *testing.T) {
	called := false
	verify := func(string) (*handlers_eks.TokenVerifyResponse, error) { called = true; return nil, nil }
	lookup := func(string) (*handlers_eks.AccessEntryRecord, error) { return nil, nil }

	st := authenticate("not-a-k8s-aws-token", verify, lookup)

	assert.False(t, st.Authenticated)
	assert.False(t, called, "must not call STS for a token that fails to decode")
}

func TestAuthenticate_DeniesWhenVerifyFails(t *testing.T) {
	verify := func(string) (*handlers_eks.TokenVerifyResponse, error) {
		return nil, errors.New("signature mismatch")
	}
	lookup := func(string) (*handlers_eks.AccessEntryRecord, error) {
		t.Fatal("lookup must not run when verify fails")
		return nil, nil
	}

	st := authenticate(validToken("https://sts/?x=1"), verify, lookup)
	assert.False(t, st.Authenticated)
}

func TestAuthenticate_DeniesWhenNoAccessEntry(t *testing.T) {
	lookup := func(string) (*handlers_eks.AccessEntryRecord, error) {
		return nil, handlers_eks.ErrAccessEntryNotFound
	}

	st := authenticate(validToken("https://sts/?x=1"), okVerify, lookup)
	assert.False(t, st.Authenticated)
	assert.Empty(t, st.User.Username)
}

func TestAuthenticate_FallsBackUIDToARN(t *testing.T) {
	verify := func(string) (*handlers_eks.TokenVerifyResponse, error) {
		return &handlers_eks.TokenVerifyResponse{ARN: testARN}, nil // no UserID
	}
	lookup := func(string) (*handlers_eks.AccessEntryRecord, error) {
		return &handlers_eks.AccessEntryRecord{KubernetesUsername: testARN, KubernetesGroups: []string{"system:masters"}}, nil
	}

	st := authenticate(validToken("https://sts/?x=1"), verify, lookup)
	require.True(t, st.Authenticated)
	assert.Equal(t, testARN, st.User.UID)
}

func TestEnsureServingCert_PersistsAndReuses(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "wh.crt")
	keyPath := filepath.Join(dir, "wh.key")

	c1, pem1, err := ensureServingCert(certPath, keyPath)
	require.NoError(t, err)
	require.NotEmpty(t, pem1)
	require.NotNil(t, c1.serverTLSConfig())

	// Second call reuses the persisted pair (same cert bytes).
	_, pem2, err := ensureServingCert(certPath, keyPath)
	require.NoError(t, err)
	assert.Equal(t, pem1, pem2)
}

func TestWriteAPIServerKubeconfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wh.kubeconfig")
	certPEM := []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")

	require.NoError(t, writeAPIServerKubeconfig(path, "127.0.0.1:8443", certPEM))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	body := string(data)
	assert.Contains(t, body, "server: https://127.0.0.1:8443/authenticate")
	assert.Contains(t, body, base64.StdEncoding.EncodeToString(certPEM))
	assert.Contains(t, body, "current-context: webhook")
}
