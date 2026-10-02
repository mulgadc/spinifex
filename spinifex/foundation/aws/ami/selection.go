// Package ami provides small AWS AMI model helpers shared by service domains.
package ami

import (
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// SelectNewestImage returns the newest image ID among images, its CreationDate,
// and how many images were considered. Nil images, images with an empty ImageId
// and nil tag entries are skipped; a non-empty excludeTagKey drops images
// carrying that tag key. Returns an empty ID when nothing matches.
func SelectNewestImage(images []*ec2.Image, excludeTagKey string) (imageID, created string, matches int) {
	for _, img := range images {
		if img == nil || aws.StringValue(img.ImageId) == "" {
			continue
		}
		// DescribeImages filters have no negation, so callers that must reject a
		// tag (e.g. gpu-vendor) can only do it client-side, here.
		if excludeTagKey != "" && imageHasTagKey(img, excludeTagKey) {
			continue
		}
		matches++
		// CreationDate is a fixed-width RFC3339 timestamp, so lexicographic
		// comparison orders it correctly without parsing.
		if c := aws.StringValue(img.CreationDate); imageID == "" || c > created {
			imageID, created = aws.StringValue(img.ImageId), c
		}
	}
	return imageID, created, matches
}

// imageHasTagKey reports whether img carries a tag with the given key.
func imageHasTagKey(img *ec2.Image, key string) bool {
	return slices.ContainsFunc(img.Tags, func(t *ec2.Tag) bool {
		return t != nil && aws.StringValue(t.Key) == key
	})
}
