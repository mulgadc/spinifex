// Exercises the unexported filter compiler internals with no exported
// surface to drive them through.
//
//test:in-package
package handlers_ochrevector

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFilter_CompileEachOperator proves every Bedrock-KB operator (D9)
// compiles to its documented parameterized SQL shape, with args bound in
// order and nextParam advancing by exactly the params consumed.
func TestFilter_CompileEachOperator(t *testing.T) {
	tests := []struct {
		name       string
		filter     *Filter
		wantSQL    string
		wantArgs   []any
		wantNext   int
		startParam int
	}{
		{
			name:       "equals",
			filter:     Equals("status", "active"),
			wantSQL:    `metadata->>'status' = $1`,
			wantArgs:   []any{"active"},
			wantNext:   2,
			startParam: 1,
		},
		{
			name:       "notEquals",
			filter:     NotEquals("status", "archived"),
			wantSQL:    `metadata->>'status' != $1`,
			wantArgs:   []any{"archived"},
			wantNext:   2,
			startParam: 1,
		},
		{
			name:       "greaterThan",
			filter:     GreaterThan("price", 10.5),
			wantSQL:    `(metadata->>'price')::numeric > $1`,
			wantArgs:   []any{10.5},
			wantNext:   2,
			startParam: 1,
		},
		{
			name:       "greaterThanOrEquals",
			filter:     GreaterThanOrEquals("price", 10),
			wantSQL:    `(metadata->>'price')::numeric >= $1`,
			wantArgs:   []any{float64(10)},
			wantNext:   2,
			startParam: 1,
		},
		{
			name:       "lessThan",
			filter:     LessThan("price", 100),
			wantSQL:    `(metadata->>'price')::numeric < $1`,
			wantArgs:   []any{float64(100)},
			wantNext:   2,
			startParam: 1,
		},
		{
			name:       "lessThanOrEquals",
			filter:     LessThanOrEquals("price", 100),
			wantSQL:    `(metadata->>'price')::numeric <= $1`,
			wantArgs:   []any{float64(100)},
			wantNext:   2,
			startParam: 1,
		},
		{
			name:       "in",
			filter:     In("tag", []string{"a", "b", "c"}),
			wantSQL:    `metadata->>'tag' = ANY($1)`,
			wantArgs:   []any{[]string{"a", "b", "c"}},
			wantNext:   2,
			startParam: 1,
		},
		{
			name:       "notIn",
			filter:     NotIn("tag", []string{"x", "y"}),
			wantSQL:    `metadata->>'tag' != ALL($1)`,
			wantArgs:   []any{[]string{"x", "y"}},
			wantNext:   2,
			startParam: 1,
		},
		{
			name:       "startsWith",
			filter:     StartsWith("path", "/docs/"),
			wantSQL:    `starts_with(metadata->>'path', $1)`,
			wantArgs:   []any{"/docs/"},
			wantNext:   2,
			startParam: 1,
		},
		{
			name:       "stringContains",
			filter:     StringContains("body", "quarterly"),
			wantSQL:    `position($1 IN metadata->>'body') > 0`,
			wantArgs:   []any{"quarterly"},
			wantNext:   2,
			startParam: 1,
		},
		{
			name:       "listContains",
			filter:     ListContains("owners", "alice"),
			wantSQL:    `metadata->'owners' ? $1`,
			wantArgs:   []any{"alice"},
			wantNext:   2,
			startParam: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, args, next, err := tt.filter.compile(tt.startParam)
			require.NoError(t, err)
			assert.Equal(t, tt.wantSQL, sql)
			assert.Equal(t, tt.wantArgs, args)
			assert.Equal(t, tt.wantNext, next)
		})
	}
}

// TestFilter_CompileStartsAtArbitraryParam proves compile numbers its bound
// params from whatever startParam the caller passes -- the property Query
// depends on to continue numbering after the query vector's $1.
func TestFilter_CompileStartsAtArbitraryParam(t *testing.T) {
	sql, args, next, err := Equals("status", "active").compile(5)
	require.NoError(t, err)
	assert.Equal(t, `metadata->>'status' = $5`, sql)
	assert.Equal(t, []any{"active"}, args)
	assert.Equal(t, 6, next)
}

// TestFilter_CompileAndAll proves andAll joins every child with AND,
// parenthesized, and that param numbering stays contiguous across children.
func TestFilter_CompileAndAll(t *testing.T) {
	f := AndAll(
		Equals("category", "handbook"),
		GreaterThan("version", 2),
		StartsWith("path", "/kb/"),
	)
	sql, args, next, err := f.compile(2)
	require.NoError(t, err)
	assert.Equal(t,
		`(metadata->>'category' = $2 AND (metadata->>'version')::numeric > $3 AND starts_with(metadata->>'path', $4))`,
		sql)
	assert.Equal(t, []any{"handbook", float64(2), "/kb/"}, args)
	assert.Equal(t, 5, next)
}

// TestFilter_CompileOrAll proves orAll joins every child with OR.
func TestFilter_CompileOrAll(t *testing.T) {
	f := OrAll(Equals("category", "handbook"), Equals("category", "faq"))
	sql, args, next, err := f.compile(1)
	require.NoError(t, err)
	assert.Equal(t, `(metadata->>'category' = $1 OR metadata->>'category' = $2)`, sql)
	assert.Equal(t, []any{"handbook", "faq"}, args)
	assert.Equal(t, 3, next)
}

// TestFilter_CompileNestedCombinators proves a combinator nested inside
// another compiles correctly and keeps param numbering contiguous through
// the nesting.
func TestFilter_CompileNestedCombinators(t *testing.T) {
	f := AndAll(
		OrAll(Equals("category", "handbook"), Equals("category", "faq")),
		LessThanOrEquals("version", 5),
	)
	sql, args, next, err := f.compile(1)
	require.NoError(t, err)
	assert.Equal(t,
		`((metadata->>'category' = $1 OR metadata->>'category' = $2) AND (metadata->>'version')::numeric <= $3)`,
		sql)
	assert.Equal(t, []any{"handbook", "faq", float64(5)}, args)
	assert.Equal(t, 4, next)
}

// TestFilter_CombinatorRequiresChildren proves andAll/orAll with no
// children is a compile error, not an always-true/always-false SQL
// fragment (which could silently drop a filter caller intended).
func TestFilter_CombinatorRequiresChildren(t *testing.T) {
	_, _, _, err := AndAll().compile(1)
	require.Error(t, err)

	_, _, _, err = OrAll().compile(1)
	require.Error(t, err)
}

// TestFilter_NumericOperatorsRejectNonNumericValue proves a numeric
// comparison with a non-numeric value is a compile error, not a query that
// reaches Postgres and fails a ::numeric cast at execution time.
func TestFilter_NumericOperatorsRejectNonNumericValue(t *testing.T) {
	_, _, _, err := GreaterThan("price", "not-a-number").compile(1)
	require.Error(t, err)
}

// TestFilter_InRejectsEmptyList proves in/notIn reject an empty value list.
func TestFilter_InRejectsEmptyList(t *testing.T) {
	_, _, _, err := In("tag", nil).compile(1)
	require.Error(t, err)

	_, _, _, err = NotIn("tag", []string{}).compile(1)
	require.Error(t, err)
}

// TestFilter_ListContainsRejectsNonStringValue proves listContains rejects
// a non-string value rather than silently stringifying it.
func TestFilter_ListContainsRejectsNonStringValue(t *testing.T) {
	_, _, _, err := (&Filter{Op: FilterListContains, Key: "owners", Value: 42}).compile(1)
	require.Error(t, err)
}

// TestFilter_UnknownOperatorErrors proves an operator outside the Bedrock
// KB set is a compile error -- the filter grammar is closed, not
// extensible to arbitrary SQL via a crafted Op string.
func TestFilter_UnknownOperatorErrors(t *testing.T) {
	_, _, _, err := (&Filter{Op: "dropTable", Key: "k", Value: "v"}).compile(1)
	require.Error(t, err)
}

// TestFilter_InjectionAttemptInKeyIsRejected is the core security property
// of D9: a jsonb key has no parameterized form, so it can only ever reach
// SQL text after passing filterKeyPattern. An injection payload as a key
// must be rejected outright, for every operator that takes a key.
func TestFilter_InjectionAttemptInKeyIsRejected(t *testing.T) {
	maliciousKeys := []string{
		"status'; DROP TABLE idx_one; --",
		"status' OR '1'='1",
		"a.b",
		"a b",
		"a\"b",
		"",
		strings.Repeat("a", 65), // over the 64-char allowlist cap
	}

	for _, key := range maliciousKeys {
		t.Run(key, func(t *testing.T) {
			_, _, _, err := Equals(key, "x").compile(1)
			require.Error(t, err, "key %q must be rejected", key)

			_, _, _, err = GreaterThan(key, 1).compile(1)
			require.Error(t, err, "key %q must be rejected", key)

			_, _, _, err = ListContains(key, "x").compile(1)
			require.Error(t, err, "key %q must be rejected", key)
		})
	}
}

// TestFilter_MaliciousValueStaysBoundArg is D9's other invariant: a value
// is never interpolated into the SQL string, no matter its content -- it
// only ever appears as an element of the returned args slice.
func TestFilter_MaliciousValueStaysBoundArg(t *testing.T) {
	const payload = `'; DROP TABLE idx_one; --`

	sql, args, _, err := Equals("status", payload).compile(1)
	require.NoError(t, err)
	assert.Equal(t, `metadata->>'status' = $1`, sql, "the SQL text must be unaffected by the value")
	assert.NotContains(t, sql, payload, "the payload must never appear in the SQL string")
	require.Len(t, args, 1)
	assert.Equal(t, payload, args[0], "the payload must be preserved intact as a bound arg")

	sql, args, _, err = StringContains("body", payload).compile(1)
	require.NoError(t, err)
	assert.Equal(t, `position($1 IN metadata->>'body') > 0`, sql)
	assert.NotContains(t, sql, payload)
	require.Len(t, args, 1)
	assert.Equal(t, payload, args[0])

	sql, args, _, err = In("tag", []string{payload, "safe"}).compile(1)
	require.NoError(t, err)
	assert.Equal(t, `metadata->>'tag' = ANY($1)`, sql)
	assert.NotContains(t, sql, payload)
	require.Len(t, args, 1)
	assert.Equal(t, []string{payload, "safe"}, args[0])
}
