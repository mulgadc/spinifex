package awsmodel_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/mulgadc/spinifex/internal/awsmodel"
	"github.com/stretchr/testify/require"
)

func encodedForm(t *testing.T, request *awsmodel.EncodedRequest) url.Values {
	t.Helper()
	require.Equal(t, http.MethodPost, request.Method)
	require.Equal(t, "/", request.Path)
	form, err := url.ParseQuery(string(request.Body))
	require.NoError(t, err)
	return form
}

func TestEncodeRequestAWSQueryWrapsListMembers(t *testing.T) {
	request, err := awsmodel.EncodeRequest(awsmodel.IAM, "CreateRole", map[string]any{
		"RoleName":                 "app",
		"AssumeRolePolicyDocument": "{}",
		"MaxSessionDuration":       int64(3600),
		"Tags":                     []any{map[string]any{"Key": "team", "Value": "core"}},
	})
	require.NoError(t, err)
	require.Equal(t, url.Values{
		"Action":                   {"CreateRole"},
		"Version":                  {"2010-05-08"},
		"RoleName":                 {"app"},
		"AssumeRolePolicyDocument": {"{}"},
		"MaxSessionDuration":       {"3600"},
		"Tags.member.1.Key":        {"team"},
		"Tags.member.1.Value":      {"core"},
	}, encodedForm(t, request))
}

func TestEncodeRequestEC2UsesQueryNamesWithoutMemberWrapper(t *testing.T) {
	request, err := awsmodel.EncodeRequest(awsmodel.EC2, "RunInstances", map[string]any{
		"ImageId":          "ami-0123456789abcdef0",
		"MinCount":         int64(1),
		"MaxCount":         int64(1),
		"SecurityGroupIds": []any{"sg-0123456789abcdef0"},
		"TagSpecifications": []any{map[string]any{
			"ResourceType": "instance",
			"Tags":         []any{map[string]any{"Key": "Name", "Value": "web"}},
		}},
	})
	require.NoError(t, err)
	require.Equal(t, url.Values{
		"Action":                          {"RunInstances"},
		"Version":                         {"2016-11-15"},
		"ImageId":                         {"ami-0123456789abcdef0"},
		"MinCount":                        {"1"},
		"MaxCount":                        {"1"},
		"SecurityGroupId.1":               {"sg-0123456789abcdef0"},
		"TagSpecification.1.ResourceType": {"instance"},
		"TagSpecification.1.Tag.1.Key":    {"Name"},
		"TagSpecification.1.Tag.1.Value":  {"web"},
	}, encodedForm(t, request))
}

func TestEncodeRequestJSONTargetsOperation(t *testing.T) {
	tests := []struct {
		service   awsmodel.Service
		operation string
		target    string
	}{
		{awsmodel.ECS, "ListClusters", "AmazonEC2ContainerServiceV20141113.ListClusters"},
		{awsmodel.ECR, "DescribeRepositories", "AmazonEC2ContainerRegistry_V20150921.DescribeRepositories"},
		{awsmodel.ACM, "ListCertificates", "CertificateManager.ListCertificates"},
	}
	for _, test := range tests {
		t.Run(string(test.service), func(t *testing.T) {
			request, err := awsmodel.EncodeRequest(test.service, test.operation, map[string]any{"maxResults": int64(5)})
			if test.service == awsmodel.ACM {
				request, err = awsmodel.EncodeRequest(test.service, test.operation, map[string]any{"MaxItems": int64(5)})
			}
			require.NoError(t, err)
			require.Equal(t, http.MethodPost, request.Method)
			require.Equal(t, test.target, request.Header.Get("X-Amz-Target"))
			require.Equal(t, "application/x-amz-json-1.1", request.Header.Get("Content-Type"))
			var body map[string]any
			require.NoError(t, json.Unmarshal(request.Body, &body))
			require.Len(t, body, 1)
		})
	}
}

func TestEncodeRequestRestJSONPlacesMembers(t *testing.T) {
	describe, err := awsmodel.EncodeRequest(awsmodel.EKS, "DescribeCluster", map[string]any{"name": "prod cluster"})
	require.NoError(t, err)
	require.Equal(t, http.MethodGet, describe.Method)
	require.Equal(t, "https://eks.example/clusters/prod%20cluster", describe.URL("https://eks.example"))

	list, err := awsmodel.EncodeRequest(awsmodel.EKS, "ListClusters", map[string]any{"maxResults": int64(5), "include": []any{"all"}})
	require.NoError(t, err)
	require.Equal(t, url.Values{"maxResults": {"5"}, "include": {"all"}}, list.Query)
	require.Empty(t, list.Body)

	create, err := awsmodel.EncodeRequest(awsmodel.EKS, "CreateCluster", map[string]any{
		"name":               "prod",
		"roleArn":            "arn:aws:iam::123456789012:role/eks",
		"resourcesVpcConfig": map[string]any{"subnetIds": []any{"subnet-1"}},
	})
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, create.Method)
	require.Equal(t, "/clusters", create.Path)
	require.Equal(t, "application/json", create.Header.Get("Content-Type"))
	require.JSONEq(t, `{"name":"prod","roleArn":"arn:aws:iam::123456789012:role/eks","resourcesVpcConfig":{"subnetIds":["subnet-1"]}}`, string(create.Body))
}

func TestEncodeRequestRejectsUnmodelledMember(t *testing.T) {
	_, err := awsmodel.EncodeRequest(awsmodel.IAM, "GetRole", map[string]any{"RoleName": "app", "Bogus": "x"})
	require.ErrorContains(t, err, `no member "Bogus"`)
}

func TestEncodeRequestS3IsNotSupported(t *testing.T) {
	_, err := awsmodel.EncodeRequest(awsmodel.S3, "ListBuckets", map[string]any{})
	require.ErrorContains(t, err, "not implemented")
}
