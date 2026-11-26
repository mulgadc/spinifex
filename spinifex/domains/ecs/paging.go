package ecs

import (
	"github.com/aws/aws-sdk-go/aws"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/paging"
)

// AWS's page sizes: most List calls default to and cap at 100, ListServices
// defaults to 10, and DescribeCapacityProviders defaults to and caps at 10.
const (
	listPageMax             = 100
	listServicesPageDefault = 10
	capacityProvidersMax    = 10
)

// parseListPage validates a call's maxResults against 1-ceiling and its
// nextToken, and returns the page they select; an omitted maxResults is def.
func parseListPage(maxResults *int64, nextToken *string, def, ceiling int64) (paging.Request, error) {
	limit := def
	if maxResults != nil {
		limit = *maxResults
		if limit < 1 || limit > ceiling {
			return paging.Request{}, awserrors.Errorf(awserrors.ErrorECSInvalidParameter,
				"maxResults must be between 1 and %d.", ceiling)
		}
	}
	from, err := paging.DecodeToken(aws.StringValue(nextToken))
	if err != nil {
		return paging.Request{}, awserrors.Errorf(awserrors.ErrorECSInvalidParameter, "Invalid token %s", aws.StringValue(nextToken))
	}
	return paging.Request{Limit: int(limit), From: from}, nil
}

func identity(s string) string { return s }
