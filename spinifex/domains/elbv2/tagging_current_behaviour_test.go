package elbv2

import (
	"context"
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

// AWS's AddTags reference (API_AddTags) returns a per-type NotFound error and
// has no partial-success result, so a failed call must tag nothing: every ARN
// is resolved before any write and the earlier resource stays untagged.
func TestCurrentBehaviour_Tagging_ELBv2AddTagsAppliesNothingWhenALaterARNIsUnknown(t *testing.T) {
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

	assert.Empty(t, tagsFor(t, svc, arn), "the first resource must not be tagged")
}

// AddTags stores a key with the aws: prefix.
func TestCurrentBehaviour_Tagging_ELBv2AddTagsAcceptsAwsPrefixedKey(t *testing.T) {
	svc := setupTestService(t)
	lbOut, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{Name: aws.String("cb-limits-lb")}, testAccountID)
	require.NoError(t, err)
	arn := *lbOut.LoadBalancers[0].LoadBalancerArn

	tags := []*elbv2.Tag{{Key: aws.String("aws:reserved"), Value: aws.String("v")}}
	_, err = svc.AddTags(context.Background(), &elbv2.AddTagsInput{ResourceArns: []*string{aws.String(arn)}, Tags: tags}, testAccountID)
	require.NoError(t, err)

	assert.Contains(t, tagsFor(t, svc, arn), "aws:reserved")
}
