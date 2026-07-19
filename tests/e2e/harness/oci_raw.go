//go:build e2e

package harness

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

// OCIRequest issues a raw HTTP request against an OCI Distribution Spec v2
// registry host (see ECRRegistryHost), authenticating with bearerToken. It
// reuses c's TLS-configured HTTP client so certificate trust matches the AWS
// SDK clients built alongside it. Unlike the AWS SDK calls in this package,
// the OCI surface is not SigV4-signed — the gateway authenticates it purely
// via the Bearer JWT — so this issues the request directly rather than
// through a signer.
//
// t.Fatal fires only on a transport-level failure (DNS, connection refused,
// TLS handshake); HTTP-level outcomes (401/403/404/...) are returned for the
// caller to assert on, since that is exactly what the IAM authorization
// matrix tests exercise.
func OCIRequest(t *testing.T, c *AWSClient, method, host, path, bearerToken string, body []byte) (status int, headers http.Header, respBody []byte) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, "https://"+host+path, reader)
	if err != nil {
		t.Fatalf("OCIRequest: build request: %v", err)
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	httpClient := c.EC2Conf.Config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("OCIRequest: %s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("OCIRequest: read body: %v", err)
	}
	return resp.StatusCode, resp.Header, respBody
}
