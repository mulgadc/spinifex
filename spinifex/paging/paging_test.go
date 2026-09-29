package paging

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func identity(s string) string { return s }

func TestPage_WalksEveryItemOnceInKeyOrder(t *testing.T) {
	items := []string{"d", "a", "e", "c", "b"}
	var seen []string
	req := Request{Limit: 2}
	for range len(items) {
		page, next := Page(items, identity, req)
		seen = append(seen, page...)
		if next == "" {
			break
		}
		from, err := DecodeToken(next)
		require.NoError(t, err)
		req.From = from
	}
	assert.Equal(t, []string{"a", "b", "c", "d", "e"}, seen)
}

func TestPage_ResumesPastADeletedItem(t *testing.T) {
	_, next := Page([]string{"a", "b", "c", "d"}, identity, Request{Limit: 2})
	from, err := DecodeToken(next)
	require.NoError(t, err)
	require.Equal(t, "c", from)

	page, _ := Page([]string{"a", "b", "d"}, identity, Request{Limit: 2, From: from})
	assert.Equal(t, []string{"d"}, page)
}

func TestPage_NoLimitReturnsEverythingWithoutToken(t *testing.T) {
	page, next := Page([]string{"b", "a"}, identity, Request{})
	assert.Equal(t, []string{"a", "b"}, page)
	assert.Empty(t, next)

	page, next = Page([]string{"a", "b"}, identity, Request{Limit: 2})
	assert.Len(t, page, 2)
	assert.Empty(t, next, "an exactly full last page carries no token")
}

func TestDecodeToken_RejectsTokensItDidNotIssue(t *testing.T) {
	for _, token := range []string{
		"garbage!",
		base64.RawURLEncoding.EncodeToString([]byte("subnet-123")),
		base64.RawURLEncoding.EncodeToString([]byte(tokenPrefix)),
	} {
		_, err := DecodeToken(token)
		assert.ErrorIs(t, err, ErrInvalidToken, token)
	}
}

func requireAWSError(t *testing.T, err error, code, message string) {
	t.Helper()
	got, msg, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "error %v carries no AWS code", err)
	assert.Equal(t, code, got)
	assert.Equal(t, message, msg)
}

func TestEC2Parse(t *testing.T) {
	p := EC2{
		MaxResults: 1000,
		TooLarge:   "too large %d",
		TooSmall:   "too small %d",
		WithIDs:    "ids and max",
	}

	req, err := p.Parse(aws.Int64(1000), aws.String(EncodeToken("subnet-b")), 0)
	require.NoError(t, err)
	assert.Equal(t, Request{Limit: 1000, From: "subnet-b"}, req)

	req, err = p.Parse(nil, nil, 2)
	require.NoError(t, err, "IDs without MaxResults are fine")
	assert.Equal(t, Request{}, req)

	_, err = p.Parse(aws.Int64(5), nil, 0)
	require.NoError(t, err)

	_, err = p.Parse(aws.Int64(1001), nil, 0)
	requireAWSError(t, err, awserrors.ErrorInvalidParameterValue, "too large 1001")
	for _, n := range []int64{4, 0, -1} {
		_, err = p.Parse(aws.Int64(n), nil, 0)
		requireAWSError(t, err, awserrors.ErrorInvalidParameterValue, fmt.Sprintf("too small %d", n))
	}

	_, err = p.Parse(aws.Int64(2000), nil, 1)
	requireAWSError(t, err, awserrors.ErrorInvalidParameterValue, "too large 2000")
	_, err = p.Parse(aws.Int64(5), nil, 1)
	requireAWSError(t, err, awserrors.ErrorInvalidParameterCombination, "ids and max")

	_, err = p.Parse(nil, aws.String("garbage"), 0)
	requireAWSError(t, err, awserrors.ErrorInvalidParameterValue,
		"Value ( garbage ) for parameter NextToken is invalid. The token is invalid.")

	p.PaginationTokenError = true
	_, err = p.Parse(nil, aws.String("garbage"), 0)
	requireAWSError(t, err, awserrors.ErrorInvalidPaginationToken, "Next token 'garbage' is invalid")
}
