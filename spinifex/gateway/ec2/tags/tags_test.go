package gateway_ec2_tags

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

// Handler tests — call handlers directly to cover validation + NATS error paths

func TestCreateTags_ValidationErrors(t *testing.T) {
	_, err := CreateTags(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)

	_, err = CreateTags(context.Background(), &ec2.CreateTagsInput{}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)

	_, err = CreateTags(context.Background(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-123")},
	}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)

	_, err = CreateTags(context.Background(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-123")},
		Tags:      []*ec2.Tag{{Key: aws.String(""), Value: aws.String("v")}},
	}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestCreateTags_NilNATS(t *testing.T) {
	_, err := CreateTags(context.Background(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-1234567890abcdef0")},
		Tags:      []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("test")}},
	}, nil, "acct-123")
	assert.Error(t, err)
}

func TestDeleteTags_ValidationErrors(t *testing.T) {
	_, err := DeleteTags(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)

	_, err = DeleteTags(context.Background(), &ec2.DeleteTagsInput{}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDeleteTags_NilNATS(t *testing.T) {
	_, err := DeleteTags(context.Background(), &ec2.DeleteTagsInput{
		Resources: []*string{aws.String("i-1234567890abcdef0")},
	}, nil, "acct-123")
	assert.Error(t, err)
}

func TestDescribeTags_NilNATS(t *testing.T) {
	_, err := DescribeTags(context.Background(), nil, nil, "acct-123")
	assert.Error(t, err)

	_, err = DescribeTags(context.Background(), &ec2.DescribeTagsInput{}, nil, "acct-123")
	assert.Error(t, err)
}
