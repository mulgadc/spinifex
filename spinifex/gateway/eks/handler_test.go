package gateway_eks

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateEKSErrorResponse_ShapesExceptionSuffix(t *testing.T) {
	body := GenerateEKSErrorResponse("ResourceNotFound", "Cluster does not exist")
	var env EKSJSONError
	require.NoError(t, json.Unmarshal(body, &env))
	assert.Equal(t, "ResourceNotFoundException", env.Type)
	assert.Equal(t, "Cluster does not exist", env.Message)
}

// Codes that already carry the "Exception" suffix (e.g.
// awserrors.ErrorEKSResourceNotFound = "ResourceNotFoundException") must not be
// doubled into ResourceNotFoundExceptionException, which SDK clients reject.
func TestGenerateEKSErrorResponse_DoesNotDoubleExceptionSuffix(t *testing.T) {
	body := GenerateEKSErrorResponse("ResourceNotFoundException", "Cluster does not exist")
	var env EKSJSONError
	require.NoError(t, json.Unmarshal(body, &env))
	assert.Equal(t, eks.ErrCodeResourceNotFoundException, env.Type)
}

func TestWriteJSONError_SetsContentTypeAndStatus(t *testing.T) {
	w := httptest.NewRecorder()
	WriteJSONError(w, "NotImplemented", "Operation not implemented", http.StatusNotImplemented)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
	assert.Equal(t, JSONContentType, w.Header().Get("Content-Type"))

	var env EKSJSONError
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "NotImplementedException", env.Type)
	assert.Equal(t, "Operation not implemented", env.Message)
}

func TestWriteJSONResponse_SerializesObject(t *testing.T) {
	w := httptest.NewRecorder()
	WriteJSONResponse(w, map[string]string{"foo": "bar"})
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, JSONContentType, w.Header().Get("Content-Type"))

	var got map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "bar", got["foo"])
}

// TestWriteJSONResponse_RestJSONWireShape guards against regressing to
// encoding/json, which ignores aws-sdk-go's locationName tags and emits Go
// PascalCase keys the AWS SDK cannot parse. The body must carry the lowercase
// restjson field names, nested all the way down.
func TestWriteJSONResponse_RestJSONWireShape(t *testing.T) {
	w := httptest.NewRecorder()
	out := &eks.DescribeClusterOutput{
		Cluster: &eks.Cluster{
			Name:     aws.String("smoke"),
			Status:   aws.String("ACTIVE"),
			Endpoint: aws.String("https://internal-eks-smoke:443"),
			Identity: &eks.Identity{
				Oidc: &eks.OIDC{Issuer: aws.String("https://gw/oidc/eks/x")},
			},
		},
	}
	WriteJSONResponse(w, out)
	require.Equal(t, http.StatusOK, w.Code)

	body := w.Body.String()
	for _, k := range []string{`"cluster"`, `"name"`, `"status"`, `"endpoint"`, `"identity"`, `"oidc"`, `"issuer"`} {
		assert.Contains(t, body, k, "restjson body missing lowercase key %s", k)
	}
	for _, k := range []string{`"Cluster"`, `"Status"`, `"Endpoint"`, `"Identity"`, `"Issuer"`} {
		assert.NotContains(t, body, k, "restjson body leaked PascalCase key %s — encoding/json regression", k)
	}

	// The AWS SDK round-trips through the same restjson nested keys.
	var got struct {
		Cluster struct {
			Status   string `json:"status"`
			Endpoint string `json:"endpoint"`
			Identity struct {
				Oidc struct {
					Issuer string `json:"issuer"`
				} `json:"oidc"`
			} `json:"identity"`
		} `json:"cluster"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "ACTIVE", got.Cluster.Status)
	assert.Equal(t, "https://internal-eks-smoke:443", got.Cluster.Endpoint)
	assert.Equal(t, "https://gw/oidc/eks/x", got.Cluster.Identity.Oidc.Issuer)
}

func TestWriteJSONResponse_ListClustersWireShape(t *testing.T) {
	w := httptest.NewRecorder()
	WriteJSONResponse(w, &eks.ListClustersOutput{
		Clusters:  aws.StringSlice([]string{"alpha", "beta"}),
		NextToken: aws.String("tok"),
	})
	body := w.Body.String()
	assert.Contains(t, body, `"clusters"`)
	assert.Contains(t, body, `"nextToken"`)
	assert.NotContains(t, body, `"Clusters"`)
	assert.NotContains(t, body, `"NextToken"`)
}

type sampleInput struct {
	Name string `json:"name"`
}

func TestParseJSONBody_EmptyBodyOK(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	got, err := ParseJSONBody[sampleInput](req)
	require.NoError(t, err)
	assert.Empty(t, got.Name)
}

func TestParseJSONBody_DecodesBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/clusters", bytes.NewReader([]byte(`{"name":"alpha"}`)))
	got, err := ParseJSONBody[sampleInput](req)
	require.NoError(t, err)
	assert.Equal(t, "alpha", got.Name)
}

func TestParseJSONBody_InvalidJSONErrors(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/clusters", bytes.NewReader([]byte(`{not-json}`)))
	_, err := ParseJSONBody[sampleInput](req)
	require.Error(t, err)
}
