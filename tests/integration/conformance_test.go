//go:build integration

package integration

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/awsmodel"
	"github.com/stretchr/testify/require"
)

func TestConformanceMiddlewareDetectsStubbedNATSMalformedResponse(t *testing.T) {
	collector := newConformanceCollector()
	gw := startGateway(t, collector)
	stubEmptyInstanceBuckets(t, gw)
	gw.StubSubject(t, "ec2.DescribeInstances", mustMarshal(t, &ec2.DescribeInstancesOutput{
		Reservations: []*ec2.Reservation{{
			Instances: []*ec2.Instance{{
				State: &ec2.InstanceState{Name: aws.String("paused")},
			}},
		}},
	}))

	_, err := gw.EC2Client(t).DescribeInstances(&ec2.DescribeInstancesInput{})
	require.NoError(t, err)

	checked, violations, diagnostics := collector.counts()
	require.Equal(t, 1, checked)
	require.Equal(t, 1, violations)
	require.Zero(t, diagnostics)
	require.Contains(t, collector.report(conformancePolicy{}, conformanceModeWarn), "$.Reservations[0].Instances[0].State.Name enum")
}

func TestConformanceRatchetBlocksPromotedService(t *testing.T) {
	collector := newConformanceCollector()
	collector.record(awsmodel.STS, "GetCallerIdentity", []byte(`<GetCallerIdentityResponse><GetCallerIdentityResult>
  <UserId>user-1</UserId><Account>123456789012</Account>
  <Arn>arn:aws:iam::123456789012:user/alice</Arn><Invented>true</Invented>
</GetCallerIdentityResult></GetCallerIdentityResponse>`))
	policy := conformancePolicyFor(awsmodel.STS)

	require.Equal(t, 1, collector.blocking(policy, conformanceModeFail))
	report := collector.report(policy, conformanceModeFail)
	require.Contains(t, report, "blocking=1 promoted=sts")
	require.Contains(t, report, "CHECKED sts success=1 errors=0 unmodelled_errors=0 promoted=true")
	require.Contains(t, report, "FAIL sts GetCallerIdentity $.Invented unknown_field")
}

func TestConformanceRatchetWarnModeDoesNotBlock(t *testing.T) {
	collector := newConformanceCollector()
	collector.record(awsmodel.STS, "GetCallerIdentity", []byte(`<GetCallerIdentityResponse><GetCallerIdentityResult>
  <Invented>true</Invented>
</GetCallerIdentityResult></GetCallerIdentityResponse>`))
	policy := conformancePolicyFor(awsmodel.STS)

	require.Zero(t, collector.blocking(policy, conformanceModeWarn))
	report := collector.report(policy, conformanceModeWarn)
	require.Contains(t, report, "blocking=0 promoted=sts")
	require.Contains(t, report, "WARN sts GetCallerIdentity $.Invented unknown_field")
	require.NotContains(t, report, "FAIL sts")
}

func TestConformanceRatchetDoesNotBlockUnpromotedService(t *testing.T) {
	collector := newConformanceCollector()
	collector.record(awsmodel.ECS, "DescribeTasks", []byte(`{"tasks":[{"launchType":"INVALID"}]}`))
	policy := conformancePolicyFor(awsmodel.STS)

	require.Zero(t, collector.blocking(policy, conformanceModeFail))
	require.Contains(t, collector.report(policy, conformanceModeFail), "WARN ecs DescribeTasks")
}

func TestConformanceMiddlewareAcceptsStubbedNATSConformingResponse(t *testing.T) {
	collector := newConformanceCollector()
	gw := startGateway(t, collector)
	stubEmptyInstanceBuckets(t, gw)
	gw.StubSubject(t, "ec2.DescribeInstances", mustMarshal(t, &ec2.DescribeInstancesOutput{
		Reservations: []*ec2.Reservation{{
			Instances: []*ec2.Instance{{
				State: &ec2.InstanceState{Name: aws.String("running")},
			}},
		}},
	}))

	_, err := gw.EC2Client(t).DescribeInstances(&ec2.DescribeInstancesInput{})
	require.NoError(t, err)

	checked, violations, diagnostics := collector.counts()
	require.Equal(t, 1, checked)
	require.Zero(t, violations)
	require.Zero(t, diagnostics)
}

func TestResolveConformanceRequestPreservesQueryBody(t *testing.T) {
	body := "Action=GetCallerIdentity&Version=2011-06-15"
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIATEST/20260805/us-east-1/sts/aws4_request, SignedHeaders=host, Signature=abc")

	service, operation, ok := resolveConformanceRequest(req)
	require.True(t, ok)
	require.Equal(t, awsmodel.STS, service)
	require.Equal(t, "GetCallerIdentity", operation)

	preserved, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.True(t, bytes.Equal([]byte(body), preserved))
}

func TestConformanceMiddlewareChecksDeclaredErrorResponses(t *testing.T) {
	collector := newConformanceCollector()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"__type":"InvalidParameterException"}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("X-Amz-Target", "AmazonEC2ContainerServiceV20141113.DescribeTasks")

	conformanceMiddleware(next, collector).ServeHTTP(httptest.NewRecorder(), req)
	checked, violations, diagnostics := collector.counts()
	require.Zero(t, checked)
	require.Zero(t, violations)
	require.Zero(t, diagnostics)
	require.Contains(t, collector.report(conformancePolicy{}, conformanceModeWarn),
		"CHECKED ecs success=0 errors=1 unmodelled_errors=0 promoted=false")
}

func TestConformanceMiddlewareChecksCuratedEC2ErrorResponses(t *testing.T) {
	collector := newConformanceCollector()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Response><Errors><Error><Code>AccessDenied</Code><Message>denied</Message></Error></Errors><RequestID>request-1</RequestID></Response>`))
	})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`Action=DescribeInstances&Version=2016-11-15`))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIATEST/20260805/us-east-1/ec2/aws4_request, SignedHeaders=host, Signature=abc")

	conformanceMiddleware(next, collector).ServeHTTP(httptest.NewRecorder(), req)
	checked, violations, diagnostics := collector.counts()
	require.Zero(t, checked)
	require.Equal(t, 1, violations)
	require.Zero(t, diagnostics)
	require.Contains(t, collector.report(conformancePolicy{}, conformanceModeWarn),
		`WARN ec2 DescribeInstances $error.Code error_code count=1: error code "AccessDenied" is not in the curated EC2 catalog`)
}

func TestConformanceRatchetBlocksUndeclaredErrorForPromotedService(t *testing.T) {
	collector := newConformanceCollector()
	collector.recordError(awsmodel.STS, "AssumeRole", http.StatusBadRequest, []byte(`<ErrorResponse><Error>
  <Code>InventedFailure</Code><Message>broken</Message>
</Error></ErrorResponse>`))
	policy := conformancePolicyFor(awsmodel.STS)

	require.Equal(t, 1, collector.blocking(policy, conformanceModeFail))
	report := collector.report(policy, conformanceModeFail)
	require.Contains(t, report, "CHECKED sts success=0 errors=1 unmodelled_errors=0 promoted=true")
	require.Contains(t, report, "FAIL sts AssumeRole $error.Code error_code")
}
