package passrole

import (
	"errors"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	acct      = "111122223333"
	canonical = "arn:aws:iam::" + acct + ":role/svc/app"
	asWritten = "arn:aws:iam::" + acct + ":role/app"
	target    = "svc.example.amazonaws.com"
)

type fakeResolver struct {
	role Role
	err  error
}

func (f fakeResolver) ResolveRole(string, string) (Role, error) { return f.role, f.err }

type call struct {
	service, action string
	resources       []string
	keys            iampolicy.ConditionKeys
}

func recorder(result error, calls *[]call) AuthorizeFunc {
	return func(service, action string, resources []string, keys iampolicy.ConditionKeys) error {
		*calls = append(*calls, call{service, action, resources, keys})
		return result
	}
}

var errDenied = errors.New("AccessDenied")

func TestCheck_FoundRoleAuthorizesCanonicalARN(t *testing.T) {
	var calls []call
	role, err := Check(fakeResolver{role: Role{ARN: canonical, Name: "app"}}, recorder(nil, &calls), acct, asWritten, target)
	require.NoError(t, err)
	assert.Equal(t, Role{ARN: canonical, Name: "app"}, role)
	require.Len(t, calls, 1)
	assert.Equal(t, "iam", calls[0].service)
	assert.Equal(t, "PassRole", calls[0].action)
	assert.Equal(t, []string{canonical}, calls[0].resources, "the canonical ARN is authorized, not the spelling the caller gave")
	assert.Equal(t, iampolicy.ConditionKeys{iampolicy.KeyPassedToService: target}, calls[0].keys)
}

func TestCheck_DeniedFoundRole(t *testing.T) {
	var calls []call
	_, err := Check(fakeResolver{role: Role{ARN: canonical, Name: "app"}}, recorder(errDenied, &calls), acct, asWritten, target)
	require.ErrorIs(t, err, errDenied)
}

func TestCheck_MalformedAndForeignAreRefusedBeforeAuthorization(t *testing.T) {
	for name, resolveErr := range map[string]error{"malformed": ErrMalformed, "foreign": ErrForeignAccount} {
		t.Run(name, func(t *testing.T) {
			var calls []call
			_, err := Check(fakeResolver{err: resolveErr}, recorder(nil, &calls), acct, "x", target)
			require.ErrorIs(t, err, resolveErr)
			assert.Empty(t, calls, "nothing is authorized, so nothing about the role is probed")
		})
	}
}

func TestCheck_MissingRoleIsReportedOnlyToACallerAllowedToPassIt(t *testing.T) {
	var calls []call
	_, err := Check(fakeResolver{err: ErrNotFound}, recorder(errDenied, &calls), acct, asWritten, target)
	require.ErrorIs(t, err, errDenied)
	require.NotErrorIs(t, err, ErrNotFound, "a denied caller must not learn the role is absent")
	require.Len(t, calls, 1)
	assert.Equal(t, []string{asWritten}, calls[0].resources, "an absent role is authorized as written")

	calls = nil
	_, err = Check(fakeResolver{err: ErrNotFound}, recorder(nil, &calls), acct, asWritten, target)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestCheck_OutageIsReturnedUnchangedWithoutAuthorizing(t *testing.T) {
	outage := errors.New("get role: nats: timeout")
	var calls []call
	_, err := Check(fakeResolver{err: outage}, recorder(nil, &calls), acct, asWritten, target)
	require.ErrorIs(t, err, outage)
	require.NotErrorIs(t, err, ErrNotFound)
	assert.Empty(t, calls)
}
