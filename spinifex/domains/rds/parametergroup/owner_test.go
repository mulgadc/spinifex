package parametergroup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	rdsengine "github.com/mulgadc/spinifex/spinifex/domains/rds/engine"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAccount = "123456789012"
	testGroup   = "app-params"
)

type fakeDependants struct {
	users []string
	err   error
}

func (f fakeDependants) InstancesUsing(context.Context, string, string) ([]string, error) {
	return f.users, f.err
}

func newOwner(t *testing.T, dependants Dependants) *Owner {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	b := kvstore.NewBucket(js, kvstore.Config{Name: "rds-account-" + testAccount, History: 1})
	return New(dependants, func(context.Context, string) (*kvstore.Bucket, error) { return b, nil })
}

func testFamily(t *testing.T) string {
	t.Helper()
	engine, err := rdsengine.LookupEngine("postgres")
	require.NoError(t, err)
	return engine.ParameterGroupFamily()
}

func TestKeys_SitUnderPrefix(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "db-parameter-groups/pg16/meta", MetaKey("pg16"))
	assert.Equal(t, "db-parameter-groups/pg16/params/shared_buffers", ParamKey("pg16", "shared_buffers"))
	assert.True(t, strings.HasPrefix(MetaKey("x"), Prefix()))
	assert.True(t, strings.HasPrefix(ParamKey("x", "p"), ParamsPrefix("x")))

	// A group's values must not be reachable by listing groups' meta records, or
	// a list would report one group per parameter.
	assert.False(t, strings.HasPrefix(ParamKey("x", "p"), MetaKey("x")))
}

func TestCreate_DuplicateNameIsRefusedWithoutOverwrite(t *testing.T) {
	t.Parallel()
	owner := newOwner(t, fakeDependants{})
	family := testFamily(t)
	_, err := owner.Create(t.Context(), testAccount, Spec{Name: testGroup, Family: family, Description: "first"})
	require.NoError(t, err)

	_, err = owner.Create(t.Context(), testAccount, Spec{Name: testGroup, Family: family, Description: "second"})
	assert.Equal(t, awserrors.ErrorDBParameterGroupAlreadyExists, awserrors.ValidErrorCodeFromError(err))

	rec, err := owner.Get(t.Context(), testAccount, testGroup)
	require.NoError(t, err)
	assert.Equal(t, "first", rec.Description)
}

func TestDelete_RefusesWhileDependantsNameTheGroup(t *testing.T) {
	t.Parallel()
	owner := newOwner(t, fakeDependants{users: []string{"orders-db", "users-db"}})
	family := testFamily(t)
	_, err := owner.Create(t.Context(), testAccount, Spec{Name: testGroup, Family: family, Description: "d"})
	require.NoError(t, err)
	require.NoError(t, owner.SetOverrides(t.Context(), testAccount, testGroup,
		[]Override{{Name: "shared_buffers", Value: "512MB"}}))

	err = owner.Delete(t.Context(), testAccount, testGroup)
	assert.Equal(t, awserrors.ErrorDBParameterGroupInvalidState, awserrors.ValidErrorCodeFromError(err))
	assert.ErrorContains(t, err, "orders-db, users-db")

	rec, err := owner.Get(t.Context(), testAccount, testGroup)
	require.NoError(t, err)
	assert.Equal(t, "d", rec.Description)
	overrides, err := owner.Overrides(t.Context(), testAccount, testGroup)
	require.NoError(t, err)
	assert.Len(t, overrides, 1)
}

// The in-use guard fails closed: when the instance side cannot answer, the
// group and its overrides stay rather than being deleted on an unknown answer.
func TestDelete_KeepsTheGroupWhenTheInUseCheckFails(t *testing.T) {
	t.Parallel()
	owner := newOwner(t, fakeDependants{err: errors.New("instance scan failed")})
	family := testFamily(t)
	_, err := owner.Create(t.Context(), testAccount, Spec{Name: testGroup, Family: family, Description: "d"})
	require.NoError(t, err)

	require.ErrorContains(t, owner.Delete(t.Context(), testAccount, testGroup), "instance scan failed")
	_, err = owner.Get(t.Context(), testAccount, testGroup)
	require.NoError(t, err)
}

// A default name is synthesised on read rather than written, so Get never
// materialises a record a later Create would then collide with.
func TestGet_DefaultGroupSynthesisesWithoutWriting(t *testing.T) {
	t.Parallel()
	owner := newOwner(t, fakeDependants{})
	engine, err := rdsengine.LookupEngine("postgres")
	require.NoError(t, err)
	name := engine.DefaultParameterGroupName()

	rec, err := owner.Get(t.Context(), testAccount, name)
	require.NoError(t, err)
	assert.Equal(t, name, rec.Name)
	assert.Equal(t, engine.ParameterGroupFamily(), rec.Family)

	kv, err := owner.bucket(t.Context(), testAccount)
	require.NoError(t, err)
	names, err := storedNames(t.Context(), kv)
	require.NoError(t, err)
	assert.NotContains(t, names, name, "a synthesised default group must not be written to the bucket")
}

func TestDelete_RefusesADefaultGroup(t *testing.T) {
	t.Parallel()
	owner := newOwner(t, fakeDependants{})
	engine, err := rdsengine.LookupEngine("postgres")
	require.NoError(t, err)

	err = owner.Delete(t.Context(), testAccount, engine.DefaultParameterGroupName())
	assert.Equal(t, awserrors.ErrorDBParameterGroupInvalidState, awserrors.ValidErrorCodeFromError(err))
}

func TestSetOverrides_RefusesADefaultGroup(t *testing.T) {
	t.Parallel()
	owner := newOwner(t, fakeDependants{})
	engine, err := rdsengine.LookupEngine("postgres")
	require.NoError(t, err)

	err = owner.SetOverrides(t.Context(), testAccount, engine.DefaultParameterGroupName(),
		[]Override{{Name: "shared_buffers", Value: "512MB"}})
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
}

func TestSetOverrides_UnknownGroupIsNotFoundAndWritesNothing(t *testing.T) {
	t.Parallel()
	owner := newOwner(t, fakeDependants{})
	const name = "default.bogus"

	err := owner.SetOverrides(t.Context(), testAccount, name,
		[]Override{{Name: "work_mem", Value: "16384"}})
	assert.Equal(t, awserrors.ErrorDBParameterGroupNotFound, awserrors.ValidErrorCodeFromError(err))

	kv, err := owner.bucket(t.Context(), testAccount)
	require.NoError(t, err)
	keys, err := bucketKeys(t.Context(), kv)
	require.NoError(t, err)
	for _, key := range keys {
		assert.False(t, strings.HasPrefix(key, ParamsPrefix(name)), "override key %s written for an unknown group", key)
	}
}
