package handlers_imds

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_key "github.com/mulgadc/spinifex/spinifex/handlers/ec2/key"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const imdsTestAccountID = "123456789012"

// serveCounted subscribes fn to subject and returns a counter of how many
// requests the responder actually handled — the assertion hook for the TTL
// cache (a cache hit must not reach the responder).
func serveCounted[I any, O any](t *testing.T, nc *nats.Conn, subject string, fn func(*I) (*O, error)) *int32 {
	t.Helper()
	var calls int32
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		atomic.AddInt32(&calls, 1)
		utils.ServeNATSRequest(msg, fn)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return &calls
}

func TestNATSSTSAssumer_RoundTrip(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	var gotReq handlers_sts.AssumeRoleForInstanceRequest
	calls := serveCounted(t, nc, handlers_sts.SubjectAssumeRoleForInstance,
		func(req *handlers_sts.AssumeRoleForInstanceRequest) (*sts.AssumeRoleOutput, error) {
			gotReq = *req
			return &sts.AssumeRoleOutput{Credentials: &sts.Credentials{
				AccessKeyId:     aws.String("ASIATESTKEY"),
				SecretAccessKey: aws.String("secret"),
				SessionToken:    aws.String("token"),
				Expiration:      aws.Time(time.Unix(1700000000, 0).UTC()),
			}}, nil
		})

	assumer := NewNATSSTSAssumer(nc)
	out, err := assumer.AssumeRoleForInstance(imdsTestAccountID, "arn:aws:iam::123456789012:role/app", "i-abc", 3600)
	require.NoError(t, err)
	require.NotNil(t, out.Credentials)

	assert.Equal(t, "ASIATESTKEY", aws.StringValue(out.Credentials.AccessKeyId))
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
	// The request payload must round-trip every field the responder needs.
	assert.Equal(t, handlers_sts.AssumeRoleForInstanceRequest{
		AccountID:       imdsTestAccountID,
		RoleARN:         "arn:aws:iam::123456789012:role/app",
		InstanceID:      "i-abc",
		DurationSeconds: 3600,
	}, gotReq)
}

func TestNATSProfileLookup_CachesProfile(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	calls := serveCounted(t, nc, handlers_iam.SubjectResolveInstanceProfile,
		func(req *handlers_iam.ResolveInstanceProfileRequest) (*handlers_iam.InstanceProfile, error) {
			return &handlers_iam.InstanceProfile{
				InstanceProfileName: "app",
				AccountID:           req.AccountID,
				RoleName:            "app-role",
			}, nil
		})

	lookup := NewNATSProfileLookup(nc)
	for range 3 {
		prof, err := lookup.ResolveInstanceProfile(imdsTestAccountID, "app")
		require.NoError(t, err)
		assert.Equal(t, "app-role", prof.RoleName)
	}
	// Three lookups, one round-trip: the TTL cache suppressed the repeats.
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
}

func TestNATSProfileLookup_CachesRole(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	calls := serveCounted(t, nc, handlers_iam.SubjectGetRole,
		func(req *handlers_iam.GetRoleRequest) (*iam.GetRoleOutput, error) {
			return &iam.GetRoleOutput{Role: &iam.Role{
				RoleName: req.Input.RoleName,
				Arn:      aws.String("arn:aws:iam::123456789012:role/app-role"),
			}}, nil
		})

	lookup := NewNATSProfileLookup(nc)
	for range 3 {
		out, err := lookup.GetRole(imdsTestAccountID, &iam.GetRoleInput{RoleName: aws.String("app-role")})
		require.NoError(t, err)
		assert.Equal(t, "arn:aws:iam::123456789012:role/app-role", aws.StringValue(out.Role.Arn))
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
}

func TestNATSProfileLookup_PropagatesError(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	serveCounted(t, nc, handlers_iam.SubjectResolveInstanceProfile,
		func(_ *handlers_iam.ResolveInstanceProfileRequest) (*handlers_iam.InstanceProfile, error) {
			return nil, assert.AnError
		})

	lookup := NewNATSProfileLookup(nc)
	_, err := lookup.ResolveInstanceProfile(imdsTestAccountID, "missing")
	require.Error(t, err)
}

const pubKeySubject = "imds.ec2.get_public_key"

func TestNATSPublicKeyLookup_CachesMaterial(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	var gotReq handlers_ec2_key.GetPublicKeyRequest
	calls := serveCounted(t, nc, pubKeySubject,
		func(req *handlers_ec2_key.GetPublicKeyRequest) (*handlers_ec2_key.GetPublicKeyResponse, error) {
			gotReq = *req
			return &handlers_ec2_key.GetPublicKeyResponse{OpenSSHKey: "ssh-ed25519 AAAA " + req.KeyName}, nil
		})

	lookup := NewNATSPublicKeyLookup(nc)
	for range 3 {
		got, err := lookup.GetPublicKey(imdsTestAccountID, "my-key")
		require.NoError(t, err)
		assert.Equal(t, "ssh-ed25519 AAAA my-key", got)
	}
	// Three lookups, one round-trip: the TTL cache suppressed the repeats.
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
	assert.Equal(t, handlers_ec2_key.GetPublicKeyRequest{AccountID: imdsTestAccountID, KeyName: "my-key"}, gotReq)
}

// An error result is never cached: the next call must re-issue the RPC so a
// transient miss isn't pinned as a permanent absence.
func TestNATSPublicKeyLookup_DoesNotCacheError(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	calls := serveCounted(t, nc, pubKeySubject,
		func(_ *handlers_ec2_key.GetPublicKeyRequest) (*handlers_ec2_key.GetPublicKeyResponse, error) {
			return nil, errors.New(awserrors.ErrorInvalidKeyPairNotFound)
		})

	lookup := NewNATSPublicKeyLookup(nc)
	for range 2 {
		_, err := lookup.GetPublicKey(imdsTestAccountID, "missing")
		require.Error(t, err)
		// The NotFound code must survive the round-trip verbatim: the IMDS
		// handler's 404-vs-500 split keys off this exact string, so a future
		// wrap that collapses it to InternalError flips a deleted key to 500.
		assert.Equal(t, awserrors.ErrorInvalidKeyPairNotFound, err.Error())
	}
	assert.Equal(t, int32(2), atomic.LoadInt32(calls))
}

func TestTTLCache_ExpiresEntries(t *testing.T) {
	now := time.Unix(1700000000, 0)
	c := newTTLCache[string](time.Minute)
	c.now = func() time.Time { return now }

	c.put("k", "v")
	got, ok := c.get("k")
	require.True(t, ok)
	assert.Equal(t, "v", got)

	now = now.Add(2 * time.Minute) // past the TTL
	_, ok = c.get("k")
	assert.False(t, ok)
}
