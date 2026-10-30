package credverify

import (
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRehydrate_LongLivedNeedsNoSecret(t *testing.T) {
	f := longLived(&fakeIAM{}, "dev")
	p, fail := New(f, nil).Rehydrate(testAKID, testAccount)
	require.Nil(t, fail)
	assert.Equal(t, "dev", p.Identity)
	assert.Equal(t, "AIDAdev", p.UserID)
	assert.Equal(t, "arn:aws:iam::"+testAccount+":user/dev", p.CallerARN)
	assert.Zero(t, f.decrypts.Load(), "rehydrate must not decrypt the secret")
}

func TestRehydrate_UserARNKeepsPath(t *testing.T) {
	f := longLived(&fakeIAM{getUserFn: func(string, *iam.GetUserInput) (*iam.GetUserOutput, error) {
		return &iam.GetUserOutput{User: &iam.User{
			UserName: aws.String("dev"), UserId: aws.String("AIDAdev"),
			Arn: aws.String("arn:aws:iam::" + testAccount + ":user/eng/dev"),
		}}, nil
	}}, "dev")
	p, fail := New(f, nil).Rehydrate(testAKID, testAccount)
	require.Nil(t, fail)
	assert.Equal(t, "arn:aws:iam::"+testAccount+":user/eng/dev", p.CallerARN)
}

func TestRehydrate_Rejections(t *testing.T) {
	invalid := awserrors.ErrorInvalidClientTokenId
	internal := awserrors.ErrorInternalError
	cases := []struct {
		name string
		v    *Verifier
		akid string
		acct string
		code string
	}{
		{"nil verifier", nil, testAKID, testAccount, internal},
		{"unknown prefix", New(&fakeIAM{}, nil), "AIDAX", testAccount, invalid},
		{"unknown key", New(&fakeIAM{}, nil), testAKID, testAccount, invalid},
		{"key lookup outage", New(&fakeIAM{lookupErr: errors.New("nats down")}, nil), testAKID, testAccount, internal},
		{"inactive key", New(&fakeIAM{key: &iamdomain.AccessKey{
			UserName: "dev", AccountID: testAccount, Status: "Inactive"}}, nil), testAKID, testAccount, invalid},
		{"other account", New(longLived(&fakeIAM{}, "dev"), nil), testAKID, "000000000002", invalid},
		{"deleted user", New(longLived(&fakeIAM{getUserFn: func(string, *iam.GetUserInput) (*iam.GetUserOutput, error) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}}, "dev"), nil), testAKID, testAccount, invalid},
		{"user outage", New(longLived(&fakeIAM{getUserFn: func(string, *iam.GetUserInput) (*iam.GetUserOutput, error) {
			return nil, errors.New("nats down")
		}}, "dev"), nil), testAKID, testAccount, internal},
		{"empty user record", New(longLived(&fakeIAM{getUserFn: func(string, *iam.GetUserInput) (*iam.GetUserOutput, error) {
			return &iam.GetUserOutput{}, nil
		}}, "dev"), nil), testAKID, testAccount, invalid},
		{"session without STS", New(&fakeIAM{}, nil), testSession, testAccount, internal},
		{"session unknown", New(&fakeIAM{}, &fakeSTS{}), testSession, testAccount, invalid},
		{"session expired", New(&fakeIAM{}, &fakeSTS{cred: func() *stsdomain.SessionCredential {
			c := roleSession("AROAX")
			c.ExpiresAt = time.Now().Add(-time.Minute)
			return c
		}()}), testSession, testAccount, awserrors.ErrorExpiredToken},
		{"session other account", New(&fakeIAM{}, &fakeSTS{cred: roleSession("AROAX")}), testSession, "000000000002", invalid},
		{"session principal gone", New(&fakeIAM{}, &fakeSTS{cred: roleSession("AROAX"),
			principalErr: stsdomain.ErrSessionPrincipalGone}), testSession, testAccount, invalid},
		{"session principal fault", New(&fakeIAM{}, &fakeSTS{cred: roleSession("AROAX"),
			principalErr: errors.New("kv down")}), testSession, testAccount, internal},
		{"role session without ARN", New(&fakeIAM{}, &fakeSTS{cred: func() *stsdomain.SessionCredential {
			c := roleSession("AROAX")
			c.AssumedRoleARN = ""
			return c
		}()}), testSession, testAccount, invalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, f := tc.v.Rehydrate(tc.akid, tc.acct)
			require.NotNil(t, f)
			assert.Equal(t, tc.code, f.Code)
		})
	}
}

func TestRehydrate_RoleSession(t *testing.T) {
	v := New(&fakeIAM{}, &fakeSTS{cred: roleSession("AROAX")})
	p, f := v.Rehydrate(testSession, testAccount)
	require.Nil(t, f)
	assert.Equal(t, stsdomain.PrincipalTypeAssumedRole, p.PrincipalType)
	assert.Equal(t, "AROAX", p.UserID)
	assert.Equal(t, "arn:aws:sts::000000000001:assumed-role/SharedOps/session", p.CallerARN)
}

func TestRehydrate_UserSessionUsesLiveARN(t *testing.T) {
	cred := roleSession("")
	cred.PrincipalType = stsdomain.PrincipalTypeUser
	cred.AssumedRoleARN = ""
	s := &fakeSTS{cred: cred, live: &stsdomain.SessionPrincipal{UserID: "AIDAsession", UserARN: "arn:aws:iam::000000000001:user/ops/session"}}
	p, f := New(&fakeIAM{}, s).Rehydrate(testSession, testAccount)
	require.Nil(t, f)
	assert.Equal(t, "AIDAsession", p.UserID)
	assert.Equal(t, "arn:aws:iam::000000000001:user/ops/session", p.CallerARN)
}
