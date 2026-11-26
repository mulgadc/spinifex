package launchtemplate

import (
	"maps"
	"slices"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const badTokenMessage = "Value ( garbage ) for parameter NextToken is invalid. The token is invalid."

// walkPages calls describe with each returned token until the last page, and
// requires every page before the last to be full.
func walkPages(t *testing.T, limit int, describe func(token *string) ([]string, *string)) []string {
	t.Helper()
	var seen []string
	var token *string
	for range 100 {
		page, next := describe(token)
		seen = append(seen, page...)
		if next == nil {
			require.LessOrEqual(t, len(page), limit)
			return seen
		}
		require.Len(t, page, limit, "a page before the last is full")
		token = next
	}
	t.Fatal("paging did not end")
	return nil
}

// A filtered-out template sorts inside the first page, so the walk proves the
// page is cut after the filter.
func TestDescribeLaunchTemplates_PageWalkFollowsFilters(t *testing.T) {
	svc := setupTestService(t)
	nameByID := map[string]string{}
	for i := range 11 {
		lt := createTemplate(t, svc, "page-"+strconv.Itoa(i), "t3.micro")
		nameByID[*lt.LaunchTemplateId] = *lt.LaunchTemplateName
	}
	ids := slices.Sorted(maps.Keys(nameByID))
	excluded := ids[1]
	var names []string
	for _, id := range ids {
		if id != excluded {
			names = append(names, nameByID[id])
		}
	}
	filters := []*ec2.Filter{{Name: aws.String("launch-template-name"), Values: aws.StringSlice(names)}}

	seen := walkPages(t, 4, func(token *string) ([]string, *string) {
		out, err := svc.DescribeLaunchTemplates(t.Context(), &ec2.DescribeLaunchTemplatesInput{
			Filters: filters, MaxResults: aws.Int64(4), NextToken: token,
		}, testAccountID)
		require.NoError(t, err)
		var page []string
		for _, lt := range out.LaunchTemplates {
			page = append(page, *lt.LaunchTemplateId)
		}
		return page, out.NextToken
	})
	assert.Equal(t, slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return id == excluded }), seen)

	_, err := svc.DescribeLaunchTemplates(t.Context(), &ec2.DescribeLaunchTemplatesInput{NextToken: aws.String("garbage")}, testAccountID)
	assertAWSError(t, err, awserrors.ErrorInvalidParameterValue, badTokenMessage)
}

// Versions page in numeric order, and a filtered-out version inside the first
// page does not shorten it.
func TestDescribeLaunchTemplateVersions_PageWalkFollowsFilters(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "versions", "t3.micro")
	ltID := *lt.LaunchTemplateId
	addVersion(t, svc, ltID, "m5.large", "")
	for range 10 {
		addVersion(t, svc, ltID, "t3.micro", "")
	}
	filters := []*ec2.Filter{{Name: aws.String("instance-type"), Values: aws.StringSlice([]string{"t3.micro"})}}

	seen := walkPages(t, 4, func(token *string) ([]string, *string) {
		out, err := svc.DescribeLaunchTemplateVersions(t.Context(), &ec2.DescribeLaunchTemplateVersionsInput{
			LaunchTemplateId: aws.String(ltID), Filters: filters, MaxResults: aws.Int64(4), NextToken: token,
		}, testAccountID)
		require.NoError(t, err)
		var page []string
		for _, v := range out.LaunchTemplateVersions {
			page = append(page, strconv.FormatInt(*v.VersionNumber, 10))
		}
		return page, out.NextToken
	})
	assert.Equal(t, []string{"1", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12"}, seen)

	_, err := svc.DescribeLaunchTemplateVersions(t.Context(), &ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String(ltID), NextToken: aws.String("garbage"),
	}, testAccountID)
	assertAWSError(t, err, awserrors.ErrorInvalidParameterValue, badTokenMessage)
}
