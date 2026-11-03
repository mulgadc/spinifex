package uefistore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
)

const (
	// RetainedVersions is how many of an owner's newest objects a prune keeps,
	// besides every object it is told to keep.
	RetainedVersions = 3

	// PruneTimeout bounds one prune: a list of a prefix that holds at most a
	// handful of objects, and the deletes of the few past retention.
	PruneTimeout = 10 * time.Second
)

// Prune deletes ownerID's objects other than the RetainedVersions newest and
// the keys in keep, returning the keys it deleted. A failed list deletes
// nothing; a failed delete is reported and the rest still run.
func Prune(ctx context.Context, objects objectstore.ObjectStore, bucket, ownerID string, keep ...string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, PruneTimeout)
	defer cancel()

	listed, _, err := objectstore.ListAll(ctx, objects, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(KeyPrefix(ownerID)),
	})
	if err != nil {
		return nil, fmt.Errorf("list UEFI stores of %s: %w", ownerID, err)
	}
	var owned []*s3.Object
	for _, obj := range listed {
		if obj != nil && ownsKey(ownerID, aws.StringValue(obj.Key)) {
			owned = append(owned, obj)
		}
	}
	// Newest first, with the key breaking a tie so every prune of one listing
	// agrees on which objects are the newest.
	slices.SortFunc(owned, func(a, b *s3.Object) int {
		if c := aws.TimeValue(b.LastModified).Compare(aws.TimeValue(a.LastModified)); c != 0 {
			return c
		}
		return strings.Compare(aws.StringValue(b.Key), aws.StringValue(a.Key))
	})

	var deleted []string
	var errs []error
	for i, obj := range owned {
		key := aws.StringValue(obj.Key)
		if i < RetainedVersions || slices.Contains(keep, key) {
			continue
		}
		if _, err := objects.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); err != nil && !objectstore.IsNoSuchKeyError(err) {
			errs = append(errs, fmt.Errorf("delete UEFI store %s: %w", key, err))
			continue
		}
		deleted = append(deleted, key)
	}
	return deleted, errors.Join(errs...)
}
