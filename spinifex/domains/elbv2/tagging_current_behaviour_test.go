package elbv2

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestCurrentBehaviour_Tagging_ tests pin how ELBv2 AddTags applies a
// multi-resource request today. They describe current behaviour, not the
// target design, and a failure means the behaviour changed.

// AddTags walks ResourceArns in order and writes each before resolving the
// next, so an unknown ARN after a valid one fails the call with the earlier
// resource already tagged.
func TestCurrentBehaviour_Tagging_ELBv2AddTagsPartiallyAppliesBeforeUnknownARN(t *testing.T) {
	svc := setupTestService(t)
	lbOut, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{Name: aws.String("cb-partial-lb")}, testAccountID)
	require.NoError(t, err)
	arn := *lbOut.LoadBalancers[0].LoadBalancerArn
	missing := "arn:aws:elasticloadbalancing:us-east-1:" + testAccountID + ":targetgroup/missing/0123456789abcdef"

	_, err = svc.AddTags(context.Background(), &elbv2.AddTagsInput{
		ResourceArns: []*string{aws.String(arn), aws.String(missing)},
		Tags:         []*elbv2.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorELBv2TargetGroupNotFound, err.Error())

	assert.Equal(t, map[string]string{"env": "prod"}, tagsFor(t, svc, arn), "the first resource keeps the tag")
}

// AddTags refuses only an empty key: 51 tags, a 129-character key, a
// 257-character value and an aws: key are all stored.
func TestCurrentBehaviour_Tagging_ELBv2AddTagsAppliesNoTagLimits(t *testing.T) {
	svc := setupTestService(t)
	lbOut, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{Name: aws.String("cb-limits-lb")}, testAccountID)
	require.NoError(t, err)
	arn := *lbOut.LoadBalancers[0].LoadBalancerArn

	tags := []*elbv2.Tag{
		{Key: aws.String("aws:reserved"), Value: aws.String("v")},
		{Key: aws.String(strings.Repeat("k", 129)), Value: aws.String(strings.Repeat("v", 257))},
	}
	for i := range 49 {
		tags = append(tags, &elbv2.Tag{Key: aws.String(fmt.Sprintf("key-%02d", i)), Value: aws.String("v")})
	}
	_, err = svc.AddTags(context.Background(), &elbv2.AddTagsInput{ResourceArns: []*string{aws.String(arn)}, Tags: tags}, testAccountID)
	require.NoError(t, err)

	got := tagsFor(t, svc, arn)
	assert.Len(t, got, 51)
	assert.Contains(t, got, "aws:reserved")
}
