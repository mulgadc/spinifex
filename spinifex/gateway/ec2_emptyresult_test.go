package gateway

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// render drives a single EC2 action through ec2Handler exactly as the real
// dispatch table does (see ec2Actions in ec2.go), with a stub inner handler
// standing in for the NATS round trip. This exercises the real marshal path
// (currently marshalEC2Response) without needing a live NATS connection.
func render[In any](t *testing.T, action string, out any) string {
	t.Helper()
	h := ec2Handler(func(_ context.Context, _ *In, _ *GatewayConfig, _ string) (any, error) {
		return out, nil
	})
	input, err := h.parse(map[string]string{})
	require.NoError(t, err)
	xmlOutput, err := h.dispatch(action, input, &GatewayConfig{}, "acct-123", nil)
	require.NoError(t, err)
	return string(xmlOutput)
}

// TestEC2Describe_EmptyResult_EmitsContainer asserts on the raw wire XML for
// an empty Describe result: real AWS renders the empty list container (e.g.
// <keySet></keySet>); a struct-level assertion would pass even with the
// container omitted entirely, since a nil slice unmarshals from either body.
func TestEC2Describe_EmptyResult_EmitsContainer(t *testing.T) {
	cases := []struct {
		name      string
		action    string
		container string
		render    func(t *testing.T) string
	}{
		{"DescribeKeyPairs", "DescribeKeyPairs", "keySet", func(t *testing.T) string {
			return render[ec2.DescribeKeyPairsInput](t, "DescribeKeyPairs", ec2.DescribeKeyPairsOutput{})
		}},
		{"DescribeVolumes", "DescribeVolumes", "volumeSet", func(t *testing.T) string {
			return render[ec2.DescribeVolumesInput](t, "DescribeVolumes", ec2.DescribeVolumesOutput{})
		}},
		{"DescribeSubnets", "DescribeSubnets", "subnetSet", func(t *testing.T) string {
			return render[ec2.DescribeSubnetsInput](t, "DescribeSubnets", ec2.DescribeSubnetsOutput{})
		}},
		{"DescribeTags", "DescribeTags", "tagSet", func(t *testing.T) string {
			return render[ec2.DescribeTagsInput](t, "DescribeTags", ec2.DescribeTagsOutput{})
		}},
		{"DescribeInstances", "DescribeInstances", "reservationSet", func(t *testing.T) string {
			return render[ec2.DescribeInstancesInput](t, "DescribeInstances", ec2.DescribeInstancesOutput{})
		}},
		{"DescribeSnapshots", "DescribeSnapshots", "snapshotSet", func(t *testing.T) string {
			return render[ec2.DescribeSnapshotsInput](t, "DescribeSnapshots", ec2.DescribeSnapshotsOutput{})
		}},
		{"DescribeImages", "DescribeImages", "imagesSet", func(t *testing.T) string {
			return render[ec2.DescribeImagesInput](t, "DescribeImages", ec2.DescribeImagesOutput{})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.render(t)

			// The bug: the container element is omitted entirely rather than
			// rendered empty, e.g. "<DescribeKeyPairsResponse></DescribeKeyPairsResponse>".
			openEmpty := "<" + tc.container + "></" + tc.container + ">"
			openSelfClose := "<" + tc.container + "/>"
			assert.Truef(t, strings.Contains(body, openEmpty) || strings.Contains(body, openSelfClose),
				"expected empty %s container in wire XML, got: %s", tc.container, body)

			// Response element must not be empty-bodied.
			assert.NotContainsf(t, body, "<"+tc.action+"Response></"+tc.action+"Response>",
				"response body was empty, container omitted entirely: %s", body)
		})
	}
}

// TestEC2Describe_EmitsRequestId asserts every EC2 response carries a
// <requestId>, which real AWS always includes and which botocore/CLI tooling
// relies on for diagnostics (e.g. `aws ... --debug`).
func TestEC2Describe_EmitsRequestId(t *testing.T) {
	requestIDPattern := regexp.MustCompile(`<requestId>[^<]+</requestId>`)

	body := render[ec2.DescribeKeyPairsInput](t, "DescribeKeyPairs", ec2.DescribeKeyPairsOutput{})
	assert.Regexp(t, requestIDPattern, body)

	nonEmptyBody := render[ec2.DescribeKeyPairsInput](t, "DescribeKeyPairs", ec2.DescribeKeyPairsOutput{
		KeyPairs: []*ec2.KeyPairInfo{{KeyName: aws.String("spinifex-key")}},
	})
	assert.Regexp(t, requestIDPattern, nonEmptyBody)
}

// TestEC2Describe_NonEmptyResult_Unchanged is the regression guard: a
// populated Describe result must still render its real data untouched by the
// empty-container/requestId fix.
func TestEC2Describe_NonEmptyResult_Unchanged(t *testing.T) {
	body := render[ec2.DescribeKeyPairsInput](t, "DescribeKeyPairs", ec2.DescribeKeyPairsOutput{
		KeyPairs: []*ec2.KeyPairInfo{
			{KeyName: aws.String("spinifex-key"), KeyPairId: aws.String("key-abc123")},
		},
	})

	assert.Contains(t, body, "<keyName>spinifex-key</keyName>")
	assert.Contains(t, body, "<keyPairId>key-abc123</keyPairId>")
	assert.Contains(t, body, "<keySet>")
	assert.NotContains(t, body, "<keySet></keySet>", "non-empty KeyPairs must not render as an empty container")
}
