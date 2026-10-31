package awsapi_test

import (
	"net/http/httptest"
	"testing"

	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestECRRequest_MissingTarget(t *testing.T) {
	gw := newECRGW(nil, awsapi.Deps{})
	err := gw.serveECR(httptest.NewRecorder(), setupECRRequest("", ""))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingAction, err.Error())
}

func TestECRRequest_UnknownAction(t *testing.T) {
	gw := newECRGW(nil, awsapi.Deps{})
	err := gw.serveECR(httptest.NewRecorder(),
		setupECRRequest("AmazonEC2ContainerRegistry_V20150921.MadeUpAction", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

// A registered-but-unimplemented action resolves to the 501 stub until its
// handler lands. Repository actions may use a transitional gateway adapter and
// image actions a composed registry capability, so ListRepositories stands in
// as a still-stubbed action.
func TestECRRequest_KnownActionNotImplemented(t *testing.T) {
	gw := newECRGW(nil, awsapi.Deps{})
	err := gw.serveECR(httptest.NewRecorder(),
		setupECRRequest("AmazonEC2ContainerRegistry_V20150921.ListRepositories", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
}
