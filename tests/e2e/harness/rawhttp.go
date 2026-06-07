//go:build e2e

package harness

import (
	"bytes"
	"crypto/tls"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	v4 "github.com/aws/aws-sdk-go/aws/signer/v4"
)

// PostAWSAction sends a raw application/x-www-form-urlencoded POST with
// Action=<action> + Version=2016-11-15 + caller params, signed with the same
// credentials the AWS SDK is using. Gateway rejects on auth before parsing
// the action, so signing is mandatory even for InvalidAction probes.
//
// Returns the HTTP status, raw body, and the parsed <Code> from the AWS XML
// error envelope (empty for 2xx). t.Fatal on transport / signing failure.
func PostAWSAction(t *testing.T, env *Env, c *AWSClient, action string, params map[string]string) (status int, body []byte, awsCode string) {
	t.Helper()

	endpoint := c.EC2Conf.Endpoint
	if endpoint == "" {
		t.Fatalf("PostAWSAction: EC2 endpoint not configured")
	}
	region := aws.StringValue(c.EC2Conf.Config.Region)

	form := url.Values{}
	form.Set("Action", action)
	if _, ok := params["Version"]; !ok {
		// EC2 query API version — same value the SDK injects on every call.
		// Hard-coded here because we are bypassing the SDK entirely.
		form.Set("Version", "2016-11-15")
	}
	for k, v := range params {
		form.Set(k, v)
	}
	bodyBytes := []byte(form.Encode())

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(endpoint, "/")+"/", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("PostAWSAction: build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

	creds := c.EC2Conf.Config.Credentials
	if creds == nil {
		t.Fatalf("PostAWSAction: no credentials on client")
	}
	signer := v4.NewSigner(creds)
	// SignedReader keeps the body intact for replay; v4.Sign reads-then-resets.
	if _, err := signer.Sign(req, bytes.NewReader(bodyBytes), "ec2", region, time.Now()); err != nil {
		t.Fatalf("PostAWSAction: sign: %v", err)
	}

	// Reuse the EC2 client's HTTP transport so TLS trust matches the rest of
	// the suite. Fall back to a CA-loaded transport if the embedded client
	// is using the SDK default.
	httpClient := c.EC2Conf.Config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Transport: &http.Transport{TLSClientConfig: &tls.Config{}},
			Timeout:   env.DefaultTimeout,
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("PostAWSAction: %v", err)
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("PostAWSAction: read body: %v", err)
	}
	status = resp.StatusCode

	if status >= 200 && status < 300 {
		return status, body, ""
	}
	awsCode = parseAWSXMLErrorCode(body)
	return status, body, awsCode
}

// EC2 returns either <Response><Errors><Error><Code>…</Code> or the SDK-style
// <ErrorResponse><Error><Code>…</Code> envelope. Try both shapes; the
// gateway has shipped one then the other across releases.
type ec2ErrorResponse struct {
	XMLName xml.Name `xml:"Response"`
	Errors  struct {
		Error struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Error"`
	} `xml:"Errors"`
}

type sdkErrorResponse struct {
	XMLName xml.Name `xml:"ErrorResponse"`
	Error   struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	} `xml:"Error"`
}

func parseAWSXMLErrorCode(body []byte) string {
	var e1 ec2ErrorResponse
	if err := xml.Unmarshal(body, &e1); err == nil && e1.Errors.Error.Code != "" {
		return e1.Errors.Error.Code
	}
	var e2 sdkErrorResponse
	if err := xml.Unmarshal(body, &e2); err == nil && e2.Error.Code != "" {
		return e2.Error.Code
	}
	return ""
}
