package gateway

import (
	"errors"
	"testing"
	"time"

	ecrauth "github.com/mulgadc/spinifex/spinifex/domains/ecr/auth"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each row runs the gateway's resolveECRPrincipal and credverify.Rehydrate over
// the same identity state and requires the same outcome. The account-active check
// and the token-claim cross-checks sit outside Rehydrate and are not rows here.
func TestRehydrateParity_ResolveECRPrincipal(t *testing.T) {
	const acct = ecrPrincipalTestAccount
	userARN := "arn:aws:iam::" + acct + ":user/dev"

	type state struct {
		iam         iamdomain.IAMService
		sts         stsdomain.STSService
		accessKeyID string
		accountID   string
		subject     string
	}
	user := func() (*ecrMockIAMService, state) {
		i := newECRMockIAMService()
		seedECRTestUser(i, acct, "dev", ecrPrincipalTestAKID)
		return i, state{iam: i, accessKeyID: ecrPrincipalTestAKID, accountID: acct, subject: userARN}
	}
	session := func() (*ecrMockIAMService, *ecrMockSTSService, state) {
		i, s := newECRMockIAMService(), newECRMockSTSService()
		seedECRSessionUser(i, s, acct, "dev", ecrPrincipalTestASID, time.Now().Add(time.Hour))
		return i, s, state{iam: i, sts: s, accessKeyID: ecrPrincipalTestASID, accountID: acct, subject: userARN}
	}
	role := func() (*ecrMockIAMService, *ecrMockSTSService, state) {
		i, s := newECRMockIAMService(), newECRMockSTSService()
		arn := seedECRAssumedRole(i, s, acct, "deploy", "AROATESTROLE0001", ecrPrincipalTestASID, "session-1", time.Now().Add(time.Hour))
		return i, s, state{iam: i, sts: s, accessKeyID: ecrPrincipalTestASID, accountID: acct, subject: arn}
	}

	rows := []struct {
		name  string
		build func() state
	}{
		{"long-lived user", func() state { _, st := user(); return st }},
		{"long-lived pathed user", func() state {
			i, st := user()
			i.users[acct+"|dev"].Arn = &[]string{ecrPathedUserARN}[0]
			st.subject = ecrPathedUserARN
			return st
		}},
		{"global root", func() state {
			i := newECRMockIAMService()
			seedECRTestUser(i, awsidentifiers.GlobalAccountID, "root", ecrPrincipalTestAKID)
			return state{iam: i, accessKeyID: ecrPrincipalTestAKID, accountID: awsidentifiers.GlobalAccountID,
				subject: "arn:aws:iam::" + awsidentifiers.GlobalAccountID + ":root"}
		}},
		{"unknown access key", func() state {
			st := state{iam: newECRMockIAMService(), accessKeyID: ecrPrincipalTestAKID, accountID: acct}
			return st
		}},
		{"inactive access key", func() state {
			i, st := user()
			i.accessKeys[ecrPrincipalTestAKID].Status = "Inactive"
			return st
		}},
		{"access key in another account", func() state { _, st := user(); st.accountID = "000000000099"; return st }},
		{"deleted user", func() state {
			i, st := user()
			delete(i.users, acct+"|dev")
			return st
		}},
		{"IAM outage", func() state {
			i, st := user()
			i.genericErr = errors.New("nats: no responders")
			return st
		}},
		{"nil IAM", func() state { return state{accessKeyID: ecrPrincipalTestAKID, accountID: acct} }},
		{"unrecognized prefix", func() state { _, st := user(); st.accessKeyID = "AIDAUNKNOWN000000001"; return st }},
		{"session user", func() state { _, _, st := session(); return st }},
		{"session user pathed", func() state {
			_, s, st := session()
			s.liveUserARN = ecrPathedUserARN
			st.subject = ecrPathedUserARN
			return st
		}},
		{"session expired", func() state {
			_, s, st := session()
			s.sessions[ecrPrincipalTestASID].ExpiresAt = time.Now().Add(-time.Minute)
			return st
		}},
		{"session unknown", func() state {
			_, s, st := session()
			delete(s.sessions, ecrPrincipalTestASID)
			return st
		}},
		{"session in another account", func() state { _, _, st := session(); st.accountID = "000000000099"; return st }},
		{"session user deleted", func() state {
			_, s, st := session()
			s.principalErr = stsdomain.ErrSessionPrincipalGone
			return st
		}},
		{"session user recreated", func() state {
			_, s, st := session()
			s.principalErr = stsdomain.ErrSessionPrincipalReplaced
			return st
		}},
		{"session legacy record", func() state {
			_, s, st := session()
			s.principalErr = stsdomain.ErrSessionPrincipalLegacy
			return st
		}},
		{"session continuity fault", func() state {
			_, s, st := session()
			s.principalErr = errors.New("kv: connection refused")
			return st
		}},
		{"session with nil STS", func() state { _, _, st := session(); st.sts = nil; return st }},
		{"assumed role", func() state { _, _, st := role(); return st }},
		{"assumed role legacy empty type", func() state {
			_, s, st := role()
			s.sessions[ecrPrincipalTestASID].PrincipalType = ""
			return st
		}},
		{"assumed role gone", func() state {
			_, s, st := role()
			s.principalErr = stsdomain.ErrSessionPrincipalGone
			return st
		}},
		{"assumed role expired", func() state {
			_, s, st := role()
			s.sessions[ecrPrincipalTestASID].ExpiresAt = time.Now().Add(-time.Minute)
			return st
		}},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			st := row.build()
			// The gateway reads the account from the claims; the old path never
			// had a separate argument, so claims.AccountID is the same input.
			claims := &ecrauth.Claims{AccountID: st.accountID, AccessKeyID: st.accessKeyID}
			claims.Subject = st.subject
			claims.PrincipalType = principalTypeUser
			if s, ok := st.sts.(*ecrMockSTSService); ok {
				if c := s.sessions[st.accessKeyID]; c != nil && c.PrincipalType != principalTypeUser {
					claims.PrincipalType = principalTypeAssumedRole
				}
			}

			old := &GatewayConfig{IAMService: st.iam, STSService: st.sts}
			want, oldErr := old.resolveECRPrincipal(claims)

			got, f := credverify.New(st.iam, st.sts).Rehydrate(st.accessKeyID, st.accountID)

			if oldErr != nil {
				require.NotNil(t, f, "old path rejected with %v", oldErr)
				assert.Equal(t, isECRDependencyFailure(oldErr), f.Code == awserrors.ErrorInternalError,
					"dependency/invalid class differs: old=%v new=%s", oldErr, f.Code)
				return
			}
			require.Nil(t, f, "old path accepted, new failed with %v", f)
			assert.Equal(t, want.identity, got.Identity)
			assert.Equal(t, want.accountID, got.AccountID)
			assert.Equal(t, want.principalType, got.PrincipalType)
			assert.Equal(t, want.userID, got.UserID)
			assert.Equal(t, want.userARN, got.UserARN)
			assert.Equal(t, want.assumedRoleARN, got.AssumedRoleARN)
			assert.Equal(t, want.assumedRoleID, got.AssumedRoleID)
			assert.Equal(t, want.underlyingRoleARN, got.UnderlyingRoleARN)
			assert.Equal(t, claims.Subject, got.CallerARN)

			// The gateway and credverify each build the caller ARN; the same inputs must agree.
			gwARN, err := buildCallerARN(got.AccountID, got.Identity, got.PrincipalType, got.AssumedRoleARN, got.UserARN)
			require.NoError(t, err)
			assert.Equal(t, gwARN, got.CallerARN)
		})
	}
}
