package taskdefinition_test

import (
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ecs/taskdefinition"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// app, app-a, app-c and db keep an ACTIVE revision; app-b has only an INACTIVE
// one. app-c's ACTIVE revision sits between two INACTIVE ones, and "app-a/" sorts
// before "app/" as a key, so key order is not name order.
func TestFamilies_Filters(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	kv := testutil.SeedKV(t, js, "ecs-families", nil)
	o := taskdefinition.New("ap-southeast-2")
	ctx := t.Context()

	empty, err := o.Families(ctx, kv, "", "")
	require.NoError(t, err)
	assert.NotNil(t, empty)
	assert.Empty(t, empty)

	for _, f := range []string{"db", "app-c", "app-c", "app-c", "app-b", "app-a", "app"} {
		_, err := o.Register(ctx, kv, "123456789012", taskdefinition.Spec{Family: f})
		require.NoError(t, err)
	}
	for _, f := range []string{"app-b", "app-c"} {
		_, err := o.Deregister(ctx, kv, f, 1)
		require.NoError(t, err)
	}
	_, err = o.Deregister(ctx, kv, "app-c", 3)
	require.NoError(t, err)

	cases := []struct {
		name, prefix, status string
		expect               []string
	}{
		{"unset lists every family", "", "", []string{"app", "app-a", "app-b", "app-c", "db"}},
		{"ALL", "", "ALL", []string{"app", "app-a", "app-b", "app-c", "db"}},
		{"ACTIVE needs an active revision", "", "ACTIVE", []string{"app", "app-a", "app-c", "db"}},
		{"INACTIVE has none", "", "INACTIVE", []string{"app-b"}},
		{"familyPrefix", "app", "ACTIVE", []string{"app", "app-a", "app-c"}},
		{"familyPrefix matching nothing", "web", "", []string{}},
	}
	for _, tc := range cases {
		got, err := o.Families(ctx, kv, tc.prefix, tc.status)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.expect, got, tc.name)
	}
}
