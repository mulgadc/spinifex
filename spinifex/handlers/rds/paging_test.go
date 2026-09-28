package handlers_rds

import (
	"encoding/base64"
	"fmt"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func identity(s string) string { return s }

func pageNames(n int) []string {
	names := make([]string, 0, n)
	for i := range n {
		names = append(names, fmt.Sprintf("item-%03d", i))
	}
	return names
}

// Follows every Marker to the end, returning the pages as served.
func walkPages(t *testing.T, items []string, maxRecords int64) [][]string {
	t.Helper()
	var pages [][]string
	var marker *string
	for {
		page, next, err := Page(items, identity, aws.Int64(maxRecords), marker)
		require.NoError(t, err)
		pages = append(pages, page)
		if next == nil {
			return pages
		}
		marker = next
	}
}

func TestPage_WalksEveryItemOnceInOrder(t *testing.T) {
	t.Parallel()
	items := pageNames(53)
	pages := walkPages(t, items, 20)

	require.Len(t, pages, 3)
	assert.Len(t, pages[0], 20)
	assert.Len(t, pages[1], 20)
	assert.Len(t, pages[2], 13)
	assert.Equal(t, items, slices.Concat(pages...))
}

// AWS's marker is base64 of the next row's name, and the last page carries none.
func TestPage_MarkerNamesTheNextItem(t *testing.T) {
	t.Parallel()
	items := pageNames(25)
	_, next, err := Page(items, identity, aws.Int64(20), nil)
	require.NoError(t, err)
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("item-020")), aws.StringValue(next))

	last, next, err := Page(items, identity, aws.Int64(20), next)
	require.NoError(t, err)
	assert.Equal(t, items[20:], last)
	assert.Nil(t, next)
}

func TestPage_ResumesPastAnItemDeletedBetweenPages(t *testing.T) {
	t.Parallel()
	items := pageNames(30)
	_, marker, err := Page(items, identity, aws.Int64(10), nil)
	require.NoError(t, err)

	remaining := slices.Delete(slices.Clone(items), 10, 11)
	page, _, err := Page(remaining, identity, aws.Int64(10), marker)
	require.NoError(t, err)
	assert.Equal(t, items[11:21], page)
}

func TestPage_ClampsAnUnsetOrOutOfRangeMaxRecordsToTheDefault(t *testing.T) {
	t.Parallel()
	items := pageNames(defaultMaxRecords + 5)
	for _, maxRecords := range []*int64{nil, aws.Int64(0), aws.Int64(-1), aws.Int64(500)} {
		page, next, err := Page(items, identity, maxRecords, nil)
		require.NoError(t, err)
		assert.Len(t, page, defaultMaxRecords)
		assert.NotNil(t, next)
	}
}

func TestPage_RejectsAMarkerItDidNotIssue(t *testing.T) {
	t.Parallel()
	_, _, err := Page(pageNames(5), identity, nil, aws.String("not base64!"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
}

// The compound keys the catalog listings page by must sort as the listings do,
// which a separator above '-' or '.' would break for engine names sharing a prefix.
func TestPageKey_SortsFieldByField(t *testing.T) {
	t.Parallel()
	keys := []string{
		OrderableOptionPageKey(&rds.OrderableDBInstanceOption{Engine: aws.String("aurora"), DBInstanceClass: aws.String("db.t3.micro")}),
		OrderableOptionPageKey(&rds.OrderableDBInstanceOption{Engine: aws.String("aurora-mysql"), DBInstanceClass: aws.String("db.m5.large")}),
	}
	assert.True(t, slices.IsSorted(keys))
}
