package ochre

import (
	"fmt"
	"math"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/paging"
)

// maxBedrockListResults is the largest maxResults a Bedrock list call accepts.
const maxBedrockListResults = 1000

// bedrockPageRequest validates a list call's maxResults and nextToken. An
// absent maxResults asks for every remaining item.
func bedrockPageRequest(maxResults *int64, nextToken *string) (paging.Request, error) {
	var req paging.Request
	if maxResults != nil {
		n := *maxResults
		if n < 1 || n > maxBedrockListResults {
			return req, awserrors.Errorf(awserrors.ErrorValidationException,
				"maxResults must be between 1 and %d", maxBedrockListResults)
		}
		req.Limit = int(n)
	}
	from, err := paging.DecodeToken(aws.StringValue(nextToken))
	if err != nil {
		return req, awserrors.Errorf(awserrors.ErrorValidationException, "invalid nextToken")
	}
	req.From = from
	return req, nil
}

// timeSortKey orders by t, then id, as a fixed-width string. descending
// inverts the time part so a plain ascending sort lists newest first.
func timeSortKey(t time.Time, id string, descending bool) string {
	n := t.UnixNano()
	if descending {
		n = math.MaxInt64 - n
	}
	return fmt.Sprintf("%020d/%s", n, id)
}

// pageToken returns next as a token pointer, or nil on the last page.
func pageToken(next string) *string {
	if next == "" {
		return nil
	}
	return aws.String(next)
}
