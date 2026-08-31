package objectstore

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
)

// maxListPages bounds a single listing. It exists only to turn a store that
// keeps claiming truncation into an error rather than a hang; at the default
// page size that is ten million objects, far past any real prefix.
const maxListPages = 10000

// ListAll drains a ListObjectsV2 listing to exhaustion, following
// continuation tokens until the store reports IsTruncated false. It returns
// every object and every common prefix seen across all pages, combined, so a
// delimited listing's CommonPrefixes accumulate the same way Contents does.
//
// input is never mutated; each page is requested from a shallow copy with
// only ContinuationToken replaced.
//
// Reading only the first page returns a prefix of the truth with no error,
// which for a caller deleting or selecting by this listing is worse than a
// failure: a partial deletion or an off-by-a-page "latest" pick both look
// like success without this.
func ListAll(ctx context.Context, store ObjectStore, input *s3.ListObjectsV2Input) ([]*s3.Object, []*s3.CommonPrefix, error) {
	var contents []*s3.Object
	var commonPrefixes []*s3.CommonPrefix
	var token *string

	prefix := aws.StringValue(input.Prefix)

	for range maxListPages {
		page := *input
		page.ContinuationToken = token

		result, err := store.ListObjectsV2(ctx, &page)
		if err != nil {
			return nil, nil, err
		}

		contents = append(contents, result.Contents...)
		commonPrefixes = append(commonPrefixes, result.CommonPrefixes...)

		if !aws.BoolValue(result.IsTruncated) {
			return contents, commonPrefixes, nil
		}
		// Truncated with nothing to resume from. Returning what arrived is the
		// silent short answer this whole loop exists to prevent.
		if aws.StringValue(result.NextContinuationToken) == "" {
			return nil, nil, fmt.Errorf("listing %s reported truncation with no continuation token", prefix)
		}
		token = result.NextContinuationToken
	}
	return nil, nil, fmt.Errorf("listing %s did not finish within %d pages", prefix, maxListPages)
}
