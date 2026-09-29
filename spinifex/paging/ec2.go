package paging

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// ec2MinResults is the smallest MaxResults the network Describe calls accept.
const ec2MinResults = 5

// EC2 is one EC2 Describe operation's paging contract. The messages are AWS's
// own, which differ in wording and casing between operations.
type EC2 struct {
	// MaxResults is the largest MaxResults the operation accepts.
	MaxResults int64
	// TooLarge and TooSmall answer a MaxResults outside the accepted range;
	// %d is the requested value.
	TooLarge string
	TooSmall string
	// WithIDs answers MaxResults sent together with explicit resource IDs.
	WithIDs string
	// PaginationTokenError selects the InvalidPaginationToken answer to a bad
	// token that some operations give instead of InvalidParameterValue.
	PaginationTokenError bool
}

// Parse validates MaxResults, its combination with ids explicit resource IDs,
// and NextToken, and returns the page they request.
func (p EC2) Parse(maxResults *int64, nextToken *string, ids int) (Request, error) {
	if maxResults != nil {
		n := *maxResults
		if n > p.MaxResults {
			return Request{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, p.TooLarge, n)
		}
		if n < ec2MinResults {
			return Request{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, p.TooSmall, n)
		}
		if ids > 0 {
			return Request{}, awserrors.Errorf(awserrors.ErrorInvalidParameterCombination, "%s", p.WithIDs)
		}
	}

	token := aws.StringValue(nextToken)
	from, err := DecodeToken(token)
	if errors.Is(err, ErrInvalidToken) {
		if p.PaginationTokenError {
			return Request{}, awserrors.Errorf(awserrors.ErrorInvalidPaginationToken, "Next token '%s' is invalid", token)
		}
		return Request{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"Value ( %s ) for parameter NextToken is invalid. The token is invalid.", token)
	}
	return Request{Limit: int(aws.Int64Value(maxResults)), From: from}, nil
}

// EC2Page is Page with the next token in the SDK's form: nil on the last page.
func EC2Page[T any](items []T, key func(T) string, req Request) ([]T, *string) {
	page, next := Page(items, key, req)
	if next == "" {
		return page, nil
	}
	return page, aws.String(next)
}
