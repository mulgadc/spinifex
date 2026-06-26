package handlers_elbv2

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An HTTP/HTTPS target group must default ProtocolVersion to HTTP1 and round-trip
// it through both the Create output and DescribeTargetGroups. The load balancer
// controller always sends ProtocolVersion and recreates the TG (DuplicateTargetGroupName
// loop) if Describe reads it back empty.
func TestCreateTargetGroup_ProtocolVersionDefaultsAndRoundTrips(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("pv-default-tg"),
		Protocol: aws.String(ProtocolHTTP),
		Port:     aws.Int64(80),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.TargetGroups[0].ProtocolVersion)
	assert.Equal(t, ProtocolVersionHTTP1, *out.TargetGroups[0].ProtocolVersion)

	arn := out.TargetGroups[0].TargetGroupArn
	desc, err := svc.DescribeTargetGroups(&elbv2.DescribeTargetGroupsInput{
		TargetGroupArns: []*string{arn},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.TargetGroups, 1)
	require.NotNil(t, desc.TargetGroups[0].ProtocolVersion)
	assert.Equal(t, ProtocolVersionHTTP1, *desc.TargetGroups[0].ProtocolVersion)
}

// An explicit ProtocolVersion is preserved, not overwritten by the default.
func TestCreateTargetGroup_ProtocolVersionExplicitPreserved(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:            aws.String("pv-http2-tg"),
		Protocol:        aws.String(ProtocolHTTPS),
		Port:            aws.Int64(443),
		ProtocolVersion: aws.String(ProtocolVersionHTTP2),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.TargetGroups[0].ProtocolVersion)
	assert.Equal(t, ProtocolVersionHTTP2, *out.TargetGroups[0].ProtocolVersion)
}

// An unknown ProtocolVersion is rejected.
func TestCreateTargetGroup_ProtocolVersionInvalidRejected(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:            aws.String("pv-bad-tg"),
		Protocol:        aws.String(ProtocolHTTP),
		ProtocolVersion: aws.String("HTTP9"),
	}, testAccountID)
	require.Error(t, err)
}

// NLB (TCP/UDP/TLS) target groups have no ProtocolVersion; AWS omits it and so
// must Spinifex, otherwise the controller sees spurious drift on NLB TGs.
func TestCreateTargetGroup_ProtocolVersionOmittedForNLB(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("pv-tcp-tg"),
		Protocol: aws.String(ProtocolTCP),
		Port:     aws.Int64(80),
	}, testAccountID)
	require.NoError(t, err)
	assert.Nil(t, out.TargetGroups[0].ProtocolVersion)
}
