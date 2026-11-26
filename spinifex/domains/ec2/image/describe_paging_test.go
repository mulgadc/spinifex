package image_test

import (
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit/recordingstore"
	ec2image "github.com/mulgadc/spinifex/spinifex/domains/ec2/image"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A walk of five-item pages returns each image the caller owns once, and no
// image the owner check or a filter excludes.
func TestDescribeImages_PageWalkSeesEachImageOnce(t *testing.T) {
	store := recordingstore.New()
	svc := ec2image.NewImageServiceImplWithStore(store, byIDBucket)
	want := map[string]bool{}
	for i := range 12 {
		id := "ami-page" + strconv.Itoa(i)
		putByIDAMI(t, store, byIDAMIDoc(id, byIDAccount))
		want[id] = true
	}
	excluded := byIDAMIDoc("ami-excluded", byIDAccount)
	excluded.Name = "other"
	putByIDAMI(t, store, excluded)
	putByIDAMI(t, store, byIDAMIDoc("ami-foreign", byIDOther))
	input := func(maxResults *int64, token *string) *ec2.DescribeImagesInput {
		return &ec2.DescribeImagesInput{
			Owners:     aws.StringSlice([]string{"self"}),
			Filters:    []*ec2.Filter{{Name: aws.String("name"), Values: aws.StringSlice([]string{"debian-13"})}},
			MaxResults: maxResults, NextToken: token,
		}
	}

	all, err := svc.DescribeImages(t.Context(), input(nil, nil), byIDAccount)
	require.NoError(t, err)
	assert.Len(t, all.Images, len(want), "no MaxResults returns every match")
	assert.Nil(t, all.NextToken)

	seen := map[string]int{}
	var token *string
	pages := 0
	for {
		out, err := svc.DescribeImages(t.Context(), input(aws.Int64(5), token), byIDAccount)
		require.NoError(t, err)
		require.LessOrEqual(t, len(out.Images), 5)
		for _, image := range out.Images {
			seen[aws.StringValue(image.ImageId)]++
		}
		pages++
		if out.NextToken == nil {
			break
		}
		token = out.NextToken
	}
	assert.Equal(t, 3, pages)
	require.Len(t, seen, len(want))
	for id, n := range seen {
		assert.True(t, want[id], "unexpected image %s", id)
		assert.Equal(t, 1, n, "image %s seen more than once", id)
	}
}

func TestDescribeImages_PagingValidation(t *testing.T) {
	store := recordingstore.New()
	svc := ec2image.NewImageServiceImplWithStore(store, byIDBucket)
	putByIDAMI(t, store, byIDAMIDoc(byIDImage, byIDAccount))

	_, err := svc.DescribeImages(t.Context(), &ec2.DescribeImagesInput{NextToken: aws.String("garbage")}, byIDAccount)
	code, _, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, code, "a token the service did not issue is refused")

	_, err = svc.DescribeImages(t.Context(), &ec2.DescribeImagesInput{
		MaxResults: aws.Int64(5), ImageIds: aws.StringSlice([]string{byIDImage}),
	}, byIDAccount)
	code, _, ok = awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameterCombination, code)
}
