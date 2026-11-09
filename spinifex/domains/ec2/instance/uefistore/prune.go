package uefistore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
)

// PruneMinAge is how old an object must be before a prune may delete it, so a
// write-back whose pointer move has not landed yet never loses its object.
const PruneMinAge = 10 * time.Minute

// PruneResult is what one prune of a prefix did.
type PruneResult struct {
	Deleted int
	// Young counts objects other than keep that were too new to delete, so the
	// caller prunes the prefix again later.
	Young int
}

// PruneObjects deletes every object under ownerID's prefix except keep that was
// last modified before cutoff. keep must be the key the store record points at;
// an empty or foreign keep deletes nothing. An object with no modification time
// counts as young.
func PruneObjects(ctx context.Context, objects objectstore.ObjectStore, bucket, ownerID, keep string, cutoff time.Time) (PruneResult, error) {
	var res PruneResult
	if err := checkOwnedKey(ownerID, keep); err != nil {
		return res, fmt.Errorf("uefistore: refusing to prune %s: %w", ownerID, err)
	}
	contents, _, err := objectstore.ListAll(ctx, objects, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(KeyPrefix(ownerID)),
	})
	if err != nil {
		return res, fmt.Errorf("uefistore: list %s: %w", KeyPrefix(ownerID), err)
	}
	for _, obj := range contents {
		key := aws.StringValue(obj.Key)
		if key == keep {
			continue
		}
		if obj.LastModified == nil || !obj.LastModified.Before(cutoff) {
			res.Young++
			continue
		}
		_, err := objects.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil && !objectstore.IsNoSuchKeyError(err) {
			return res, fmt.Errorf("uefistore: delete %s: %w", key, err)
		}
		res.Deleted++
	}
	return res, nil
}

// checkOwnedKey fails a key that is empty or not one object under ownerID's
// prefix.
func checkOwnedKey(ownerID, key string) error {
	if !validOwner(ownerID) {
		return fmt.Errorf("%q is not an instance or image ID", ownerID)
	}
	if key == "" {
		return errors.New("no key to keep")
	}
	if !ownsKey(ownerID, key) {
		return fmt.Errorf("%s is not under %s", key, KeyPrefix(ownerID))
	}
	return nil
}
