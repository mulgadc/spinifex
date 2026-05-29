package handlers_imds

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAssumer is a programmable stsAssumer that records call counts.
type fakeAssumer struct {
	out   *sts.AssumeRoleOutput
	err   error
	calls int
}

func (f *fakeAssumer) AssumeRoleForInstance(_, _, _ string, _ int64) (*sts.AssumeRoleOutput, error) {
	f.calls++
	return f.out, f.err
}

func assumeOutput(akid string, exp time.Time) *sts.AssumeRoleOutput {
	return &sts.AssumeRoleOutput{
		Credentials: &sts.Credentials{
			AccessKeyId:     aws.String(akid),
			SecretAccessKey: aws.String("secret"),
			SessionToken:    aws.String("token"),
			Expiration:      aws.Time(exp),
		},
	}
}

func TestCredCache_MintsAndRendersAWSShape(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	exp := now.Add(time.Hour)
	assumer := &fakeAssumer{out: assumeOutput("ASIAEXAMPLE", exp)}
	cache := newCredCache(assumer)

	eni := &eniFacts{eniID: "eni-aaa", accountID: "111122223333", instanceID: "i-123"}
	body, err := cache.get(eni, "app-role", "arn:aws:iam::111122223333:role/app-role", now)
	require.NoError(t, err)

	var cred instanceCredential
	require.NoError(t, json.Unmarshal(body, &cred))
	assert.Equal(t, "Success", cred.Code)
	assert.Equal(t, "AWS-HMAC", cred.Type)
	assert.Equal(t, "ASIAEXAMPLE", cred.AccessKeyId)
	assert.Equal(t, "secret", cred.SecretAccessKey)
	assert.Equal(t, "token", cred.Token)
	assert.Equal(t, "111122223333", cred.AccountId)
	assert.Equal(t, exp.Format(time.RFC3339), cred.Expiration)
}

func TestCredCache_ServesCachedUntilRefreshWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	exp := now.Add(time.Hour)
	assumer := &fakeAssumer{out: assumeOutput("ASIAEXAMPLE", exp)}
	cache := newCredCache(assumer)
	eni := &eniFacts{eniID: "eni-aaa", accountID: "111122223333", instanceID: "i-123"}

	_, err := cache.get(eni, "app-role", "arn:aws:iam::111122223333:role/app-role", now)
	require.NoError(t, err)
	// Still well outside the 5-minute refresh window → cached, no new mint.
	_, err = cache.get(eni, "app-role", "arn:aws:iam::111122223333:role/app-role", now.Add(50*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 1, assumer.calls, "second request inside the refresh window must hit the cache")
}

func TestCredCache_RemintsInsideRefreshWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	exp := now.Add(time.Hour)
	assumer := &fakeAssumer{out: assumeOutput("ASIAEXAMPLE", exp)}
	cache := newCredCache(assumer)
	eni := &eniFacts{eniID: "eni-aaa", accountID: "111122223333", instanceID: "i-123"}

	_, err := cache.get(eni, "app-role", "arn:aws:iam::111122223333:role/app-role", now)
	require.NoError(t, err)
	// 56 minutes in → within 5 min of expiry → re-mint.
	assumer.out = assumeOutput("ASIAREFRESHED", now.Add(2*time.Hour))
	_, err = cache.get(eni, "app-role", "arn:aws:iam::111122223333:role/app-role", now.Add(56*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 2, assumer.calls, "request inside the refresh window must re-mint")
}

func TestCredCache_PropagatesAssumeError(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	assumer := &fakeAssumer{err: errors.New("AccessDenied")}
	cache := newCredCache(assumer)
	eni := &eniFacts{eniID: "eni-aaa", accountID: "111122223333", instanceID: "i-123"}

	_, err := cache.get(eni, "app-role", "arn:aws:iam::111122223333:role/app-role", now)
	require.Error(t, err)
}

func TestCredCache_PerENIPerRoleKeying(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	exp := now.Add(time.Hour)
	assumer := &fakeAssumer{out: assumeOutput("ASIAEXAMPLE", exp)}
	cache := newCredCache(assumer)

	a := &eniFacts{eniID: "eni-aaa", accountID: "111122223333", instanceID: "i-1"}
	b := &eniFacts{eniID: "eni-bbb", accountID: "111122223333", instanceID: "i-2"}
	_, _ = cache.get(a, "app-role", "arn:aws:iam::111122223333:role/app-role", now)
	_, _ = cache.get(b, "app-role", "arn:aws:iam::111122223333:role/app-role", now)
	assert.Equal(t, 2, assumer.calls, "distinct ENIs must not share a cache entry")
}
