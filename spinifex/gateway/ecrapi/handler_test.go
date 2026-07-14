package gateway_ecrapi

import (
	"context"

	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNotImplemented(t *testing.T) {
	out, err := NotImplemented(context.Background(), nil, "123456789012", []byte("{}"))
	assert.Nil(t, out)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
}

func TestActions_CoreRegisteredAsStub(t *testing.T) {
	core := []string{
		"GetAuthorizationToken", "CreateRepository", "DeleteRepository",
		"DescribeRepositories", "ListRepositories", "BatchGetImage",
		"BatchCheckLayerAvailability", "PutImage", "InitiateLayerUpload",
		"UploadLayerPart", "CompleteLayerUpload",
	}
	for _, action := range core {
		h, ok := Actions[action]
		require.True(t, ok, "action %q should be registered", action)
		_, err := h(context.Background(), nil, "123456789012", nil)
		assert.Equal(t, awserrors.ErrorNotImplemented, err.Error(),
			"action %q should resolve to the 501 stub", action)
	}
}

func TestScanningNotSupported(t *testing.T) {
	out, err := ScanningNotSupported(context.Background(), nil, "123456789012", []byte("{}"))
	assert.Nil(t, out)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorOperationNotSupported, err.Error())
}

func TestActions_ScanSurfaceUnsupported(t *testing.T) {
	scan := []string{
		"PutImageScanningConfiguration", "GetImageScanningConfiguration",
		"StartImageScan", "DescribeImageScanFindings",
		"GetRegistryScanningConfiguration", "PutRegistryScanningConfiguration",
		"BatchGetRepositoryScanningConfiguration",
	}
	for _, action := range scan {
		h, ok := Actions[action]
		require.True(t, ok, "action %q should be registered", action)
		_, err := h(context.Background(), nil, "123456789012", nil)
		assert.Equal(t, awserrors.ErrorOperationNotSupported, err.Error(),
			"action %q should resolve to the OperationNotSupported stub", action)
	}
}

func TestWriteJSONResponse(t *testing.T) {
	type repo struct {
		RepositoryName *string `locationName:"repositoryName" type:"string"`
	}
	name := "team/app"
	w := httptest.NewRecorder()
	WriteJSONResponse(w, &repo{RepositoryName: &name})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, JSONContentType, w.Header().Get("Content-Type"))

	var decoded map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &decoded))
	assert.Equal(t, "team/app", decoded["repositoryName"])
}
