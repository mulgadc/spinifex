package queue

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const otherAcct = "222233334444"

func list(f *fixture, req ListRequest) (ListResult, error) {
	if req.Account == "" {
		req.Account, req.Region = acct, region
	}
	return f.store.List(context.Background(), req)
}

func ptr(n int) *int { return &n }

func queueNames(r ListResult) []string {
	out := make([]string, 0, len(r.Queues))
	for _, q := range r.Queues {
		out = append(out, q.Name)
	}
	return out
}

func TestListScopedToAccountAndRegion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, n := range []string{"b", "a", "c"} {
		_, err := create(f, n, nil)
		require.NoError(t, err)
	}
	_, err := f.store.Create(ctx, CreateRequest{Account: otherAcct, Region: region, Name: "a-other"})
	require.NoError(t, err)
	_, err = f.store.Create(ctx, CreateRequest{Account: acct, Region: "ap-south-1", Name: "a-region"})
	require.NoError(t, err)

	r, err := list(f, ListRequest{})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c"}, queueNames(r))
	assert.Empty(t, r.NextToken, "without MaxResults no token is returned")

	r, err = list(f, ListRequest{Account: otherAcct, Region: region})
	require.NoError(t, err)
	assert.Equal(t, []string{"a-other"}, queueNames(r))
}

func TestListPrefix(t *testing.T) {
	f := newFixture(t)
	for _, n := range []string{"orders", "orders-dlq", "Orders", "ord", "payments"} {
		_, err := create(f, n, nil)
		require.NoError(t, err)
	}
	r, err := list(f, ListRequest{Prefix: "orders"})
	require.NoError(t, err)
	assert.Equal(t, []string{"orders", "orders-dlq"}, queueNames(r), "prefix match is case-sensitive")

	r, err = list(f, ListRequest{Prefix: "zzz"})
	require.NoError(t, err)
	assert.Empty(t, r.Queues)
}

func TestListPagination(t *testing.T) {
	f := newFixture(t)
	var want []string
	for i := range 7 {
		n := fmt.Sprintf("q%d", i)
		want = append(want, n)
		_, err := create(f, n, nil)
		require.NoError(t, err)
	}
	_, err := create(f, "other", nil)
	require.NoError(t, err)

	var got []string
	var pages int
	req := ListRequest{Prefix: "q", MaxResults: ptr(3)}
	for {
		r, err := list(f, req)
		require.NoError(t, err)
		pages++
		assert.LessOrEqual(t, len(r.Queues), 3)
		got = append(got, queueNames(r)...)
		if r.NextToken == "" {
			break
		}
		req.NextToken = r.NextToken
	}
	assert.Equal(t, want, got)
	assert.Equal(t, 3, pages)

	r, err := list(f, ListRequest{Prefix: "q", MaxResults: ptr(7)})
	require.NoError(t, err)
	assert.Len(t, r.Queues, 7)
	assert.Empty(t, r.NextToken, "a page that ends the listing carries no token")
}

func TestListInvalidRequests(t *testing.T) {
	f := newFixture(t)
	for i := range 3 {
		_, err := create(f, fmt.Sprintf("q%d", i), nil)
		require.NoError(t, err)
	}
	r, err := list(f, ListRequest{MaxResults: ptr(1)})
	require.NoError(t, err)
	require.NotEmpty(t, r.NextToken)
	good := r.NextToken

	forge := func(tok listToken) string {
		b, err := json.Marshal(tok)
		require.NoError(t, err)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	bad := map[string]ListRequest{
		"negative max":         {MaxResults: ptr(-1)},
		"supplied zero max":    {MaxResults: ptr(0)},
		"max above limit":      {MaxResults: ptr(maxListResults + 1)},
		"prefix with dot":      {Prefix: "a.b"},
		"prefix too long":      {Prefix: strings.Repeat("a", maxPrefixLen+1)},
		"garbage token":        {MaxResults: ptr(1), NextToken: "!!"},
		"non-json token":       {MaxResults: ptr(1), NextToken: base64.RawURLEncoding.EncodeToString([]byte("x"))},
		"other account token":  {Account: otherAcct, Region: region, MaxResults: ptr(1), NextToken: good},
		"other region token":   {Account: acct, Region: "ap-south-1", MaxResults: ptr(1), NextToken: good},
		"other prefix token":   {Prefix: "q", MaxResults: ptr(1), NextToken: good},
		"future token version": {MaxResults: ptr(1), NextToken: forge(listToken{V: 2, Acct: acct, Region: region, After: "q"})},
		"cursor outside prefix": {Prefix: "x", MaxResults: ptr(1),
			NextToken: forge(listToken{V: 1, Acct: acct, Region: region, Prefix: "x", After: "q"})},
		"cursor not a name": {MaxResults: ptr(1), NextToken: forge(listToken{V: 1, Acct: acct, Region: region, After: "a.b"})},
	}
	for name, req := range bad {
		_, err := list(f, req)
		assert.ErrorIs(t, err, ErrInvalidListRequest, name)
	}
}

// A queue deleted between the name listing and its read is omitted; one
// created after the listing is not on this page.
func TestListConcurrentCreateAndDelete(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, n := range []string{"a", "b"} {
		_, err := create(f, n, nil)
		require.NoError(t, err)
	}
	f.fault.onKeys = func(filter string) ([]string, error, bool) {
		f.fault.onKeys = nil
		keys, err := f.fault.real.keys(ctx, filter)
		require.NoError(t, err)
		require.NoError(t, f.kv.Delete(ctx, bindingKey(acct, region, "b")))
		_, err = create(f, "c", nil)
		require.NoError(t, err)
		return keys, nil, true
	}
	r, err := list(f, ListRequest{})
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, queueNames(r))

	r, err = list(f, ListRequest{})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "c"}, queueNames(r))
}
