package awsgw_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/stretchr/testify/require"
)

func TestSigV4Auth_UnsupportedServiceIsLogged(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)
	logs := captureLogs(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"

	sum := sha256.Sum256(nil)
	payloadHash := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	require.NoError(t, v4.NewSigner().SignHTTP(context.Background(),
		aws.Credentials{AccessKeyID: gwfix.AccessKey, SecretAccessKey: gwfix.SecretKey},
		req, payloadHash, "notaservice", gwfix.Region, time.Now().UTC()))

	resp := doRequest(handler, req)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	out := logs.String()
	require.Contains(t, out, "unsupported service in credential scope")
	require.Contains(t, out, "service=notaservice")
}
