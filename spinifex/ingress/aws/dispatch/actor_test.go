package dispatch

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestActor_RoleName(t *testing.T) {
	const acct = "000000000001"
	for _, tc := range []struct {
		name  string
		actor Actor
		want  string
	}{
		{"assumed role in own account", Actor{AccountID: acct, PrincipalType: "assumed-role", PrincipalARN: "arn:aws:iam::" + acct + ":role/app"}, "app"},
		{"assumed role with a path", Actor{AccountID: acct, PrincipalType: "assumed-role", PrincipalARN: "arn:aws:iam::" + acct + ":role/svc/app"}, "app"},
		{"role in another account", Actor{AccountID: acct, PrincipalType: "assumed-role", PrincipalARN: "arn:aws:iam::000000000000:role/app"}, ""},
		{"malformed role ARN", Actor{AccountID: acct, PrincipalType: "assumed-role", PrincipalARN: "not-an-arn"}, ""},
		{"no role ARN", Actor{AccountID: acct, PrincipalType: "assumed-role"}, ""},
		{"user never has a role", Actor{AccountID: acct, PrincipalType: "user", PrincipalARN: "arn:aws:iam::" + acct + ":role/app"}, ""},
		{"user ARN is not a role", Actor{AccountID: acct, PrincipalType: "assumed-role", PrincipalARN: "arn:aws:iam::" + acct + ":user/alice"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.actor.RoleName())
		})
	}
}

func TestActor_Caller(t *testing.T) {
	const acct = "000000000001"
	session := Actor{
		AccountID: acct, PrincipalType: PrincipalTypeAssumedRole,
		PrincipalARN: "arn:aws:iam::" + acct + ":role/real-role", SessionName: "i-abc", UserName: "ignored",
	}
	assert.Equal(t, Caller{AccountID: acct, PrincipalType: PrincipalTypeAssumedRole, RoleName: "real-role", SessionName: "i-abc"}, session.Caller())

	// A session named after a role does not make the caller that role.
	named := Actor{
		AccountID: acct, PrincipalType: PrincipalTypeAssumedRole,
		PrincipalARN: "arn:aws:iam::" + acct + ":role/tenant", SessionName: "real-role",
	}
	assert.Equal(t, "tenant", named.Caller().RoleName)

	user := Actor{AccountID: acct, PrincipalType: "user", UserName: "alice", SessionName: "ignored", PrincipalARN: "arn:aws:iam::" + acct + ":user/alice"}
	assert.Equal(t, Caller{AccountID: acct, PrincipalType: "user", SessionName: "alice"}, user.Caller())
}
