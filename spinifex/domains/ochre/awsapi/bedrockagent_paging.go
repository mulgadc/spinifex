package awsapi

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrockagent"
	ochrevector "github.com/mulgadc/spinifex/spinifex/domains/ochre/vector"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/paging"
)

// bedrockAgentMaxResults is the largest maxResults the bedrock-agent List
// actions accept, and the page size when maxResults is omitted.
const bedrockAgentMaxResults = 1000

// sortTimeLayout renders a time as a fixed-width string that sorts in time order.
const sortTimeLayout = "20060102T150405.000000000"

// decodeBedrockAgentBody decodes a REST-JSON request body into input; an empty
// body leaves input as it is.
func decodeBedrockAgentBody(body []byte, input any) error {
	if len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, input); err != nil {
		return awserrors.Errorf(awserrors.ErrorValidationException, "The request body is not valid JSON.")
	}
	return nil
}

// parseBedrockAgentPage validates maxResults against 1-1000 and nextToken, and
// returns the page they select.
func parseBedrockAgentPage(maxResults *int64, nextToken *string) (paging.Request, error) {
	limit := int64(bedrockAgentMaxResults)
	if maxResults != nil {
		limit = *maxResults
		if limit < 1 || limit > bedrockAgentMaxResults {
			return paging.Request{}, awserrors.Errorf(awserrors.ErrorValidationException,
				"1 validation error detected: Value '%d' at 'maxResults' failed to satisfy constraint: Member must have value between 1 and %d", limit, bedrockAgentMaxResults)
		}
	}
	from, err := paging.DecodeToken(aws.StringValue(nextToken))
	if err != nil {
		return paging.Request{}, awserrors.Errorf(awserrors.ErrorValidationException, "The nextToken %q is not valid.", aws.StringValue(nextToken))
	}
	return paging.Request{Limit: int(limit), From: from}, nil
}

// createdKey orders records by creation time, then id.
func createdKey(created time.Time, id string) string {
	return created.UTC().Format(sortTimeLayout) + "/" + id
}

// invertDigits reverses the sort order of a fixed-width digit string.
func invertDigits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return '9' - r + '0'
		}
		return r
	}, s)
}

// ingestionJobStatusFilter returns the statuses filters asks for, and whether
// a filter was given at all.
func ingestionJobStatusFilter(filters []*bedrockagent.IngestionJobFilter) ([]string, bool, error) {
	if filters == nil {
		return nil, false, nil
	}
	if len(filters) != 1 || filters[0] == nil {
		return nil, false, awserrors.Errorf(awserrors.ErrorValidationException,
			"1 validation error detected: Value at 'filters' failed to satisfy constraint: Member must have length between 1 and 1")
	}
	f := filters[0]
	if aws.StringValue(f.Attribute) != bedrockagent.IngestionJobFilterAttributeStatus {
		return nil, false, awserrors.Errorf(awserrors.ErrorValidationException,
			"1 validation error detected: Value '%s' at 'filters.1.member.attribute' failed to satisfy constraint: Member must satisfy enum value set: [STATUS]", aws.StringValue(f.Attribute))
	}
	if aws.StringValue(f.Operator) != bedrockagent.IngestionJobFilterOperatorEq {
		return nil, false, awserrors.Errorf(awserrors.ErrorValidationException,
			"1 validation error detected: Value '%s' at 'filters.1.member.operator' failed to satisfy constraint: Member must satisfy enum value set: [EQ]", aws.StringValue(f.Operator))
	}
	if len(f.Values) > 10 {
		return nil, false, awserrors.Errorf(awserrors.ErrorValidationException,
			"1 validation error detected: Value at 'filters.1.member.values' failed to satisfy constraint: Member must have length less than or equal to 10")
	}
	return aws.StringValueSlice(f.Values), true, nil
}

// ingestionJobSortStatuses is the STATUS sort order: the stored statuses in name order.
var ingestionJobSortStatuses = []string{
	bedrockagent.IngestionJobStatusComplete,
	bedrockagent.IngestionJobStatusFailed,
	bedrockagent.IngestionJobStatusInProgress,
	bedrockagent.IngestionJobStatusStarting,
	ingestionJobStatusStopped,
}

// ingestionJobSortKey returns the paging key that orders jobs as sortBy asks.
// With no sortBy, jobs sort by start time, oldest first.
func ingestionJobSortKey(sortBy *bedrockagent.IngestionJobSortBy) (func(ochrevector.JobRecord) string, error) {
	attr := bedrockagent.IngestionJobSortByAttributeStartedAt
	order := bedrockagent.SortOrderAscending
	if sortBy != nil {
		attr, order = aws.StringValue(sortBy.Attribute), aws.StringValue(sortBy.Order)
	}
	if !slices.Contains(bedrockagent.IngestionJobSortByAttribute_Values(), attr) {
		return nil, awserrors.Errorf(awserrors.ErrorValidationException,
			"1 validation error detected: Value '%s' at 'sortBy.attribute' failed to satisfy constraint: Member must satisfy enum value set: [STATUS, STARTED_AT]", attr)
	}
	if !slices.Contains(bedrockagent.SortOrder_Values(), order) {
		return nil, awserrors.Errorf(awserrors.ErrorValidationException,
			"1 validation error detected: Value '%s' at 'sortBy.order' failed to satisfy constraint: Member must satisfy enum value set: [ASCENDING, DESCENDING]", order)
	}
	desc := order == bedrockagent.SortOrderDescending
	return func(job ochrevector.JobRecord) string {
		key := job.CreatedAt.UTC().Format(sortTimeLayout)
		if attr == bedrockagent.IngestionJobSortByAttributeStatus {
			key = fmt.Sprintf("%02d", slices.Index(ingestionJobSortStatuses, jobStateToAWS(job.State))) + "/" + key
		}
		if desc {
			key = invertDigits(key)
		}
		return key + "/" + job.ID
	}, nil
}
