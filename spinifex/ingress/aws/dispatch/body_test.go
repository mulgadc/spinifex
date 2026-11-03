package dispatch_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, http.ErrBodyReadAfterClose }

func post(body io.Reader) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/", body)
}

func oversized() io.Reader {
	return strings.NewReader(strings.Repeat("a", int(sigv4.MaxPayloadLen)+1))
}

func TestResolveQueryAction_ReadsActionAndRewindsBody(t *testing.T) {
	body := "Action=DescribeVpcs&Version=2016-11-15"
	r := post(strings.NewReader(body))

	assert.Equal(t, "DescribeVpcs", dispatch.ResolveQueryAction(r))
	again, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	assert.Equal(t, body, string(again), "the dispatcher must still see the whole body")
}

func TestResolveQueryAction_FailureIsEmpty(t *testing.T) {
	for name, body := range map[string]io.Reader{
		"no action":  strings.NewReader("Version=2016-11-15"),
		"malformed":  strings.NewReader("Action=%zz"),
		"unreadable": errReader{},
		"oversized":  oversized(),
	} {
		assert.Empty(t, dispatch.ResolveQueryAction(post(body)), name)
	}
}

func TestReadArgs_OversizedBodyIsRequestEntityTooLarge(t *testing.T) {
	_, err := dispatch.ReadArgs(post(oversized()))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorRequestEntityTooLarge, err.Error())
}

func TestReadBoundedBody(t *testing.T) {
	body, err := dispatch.ReadBoundedBody(post(strings.NewReader(`{"a":1}`)))
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":1}`, string(body))

	_, err = dispatch.ReadBoundedBody(post(oversized()))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorRequestEntityTooLarge, err.Error())

	_, err = dispatch.ReadBoundedBody(post(errReader{}))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestActionFromTarget(t *testing.T) {
	for target, want := range map[string]string{
		"CertificateManager.ListCertificates": "ListCertificates",
		"a.b.ListCertificates":                "ListCertificates",
		"ListCertificates":                    "ListCertificates",
		"CertificateManager.":                 "",
		"":                                    "",
	} {
		assert.Equal(t, want, dispatch.ActionFromTarget(target), "target %q", target)
	}
}

func TestResolveTargetAction(t *testing.T) {
	r := post(nil)
	r.Header.Set("X-Amz-Target", "AmazonEC2ContainerServiceV20141113.RunTask")
	assert.Equal(t, "RunTask", dispatch.ResolveTargetAction(r))
}
