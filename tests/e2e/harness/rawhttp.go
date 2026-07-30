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

// A query-protocol service, as the signer and the wire both need it: the SigV4
// service name and the API version the gateway parses actions against.
type queryService struct {
	name    string
	version string
}

var (
	ec2QueryService = queryService{name: "ec2", version: "2016-11-15"}
	rdsQueryService = queryService{name: "rds", version: "2014-10-31"}
)

// PostAWSAction sends a raw EC2 query POST. See postQueryAction.
func PostAWSAction(t *testing.T, env *Env, c *AWSClient, action string, params map[string]string) (status int, body []byte, awsCode string) {
	t.Helper()
	return postQueryAction(t, env, c, ec2QueryService, action, params)
}

// PostRDSAction is PostAWSAction against the RDS surface: a different signing
// service and API version, so an EC2-signed request would be rejected on auth
// before the action was ever read.
func PostRDSAction(t *testing.T, env *Env, c *AWSClient, action string, params map[string]string) (status int, body []byte, awsCode string) {
	t.Helper()
	return postQueryAction(t, env, c, rdsQueryService, action, params)
}

// postQueryAction sends a raw application/x-www-form-urlencoded POST with
// Action=<action> + the service's Version + caller params, signed with the same
// credentials the AWS SDK is using. Gateway rejects on auth before parsing the
// action, so signing is mandatory even for InvalidAction probes.
//
// Returns the HTTP status, raw body, and the parsed <Code> from the AWS XML
// error envelope (empty for 2xx). t.Fatal on transport / signing failure.
func postQueryAction(t *testing.T, env *Env, c *AWSClient, svc queryService, action string, params map[string]string) (status int, body []byte, awsCode string) {
	t.Helper()

	// Every service shares one gateway endpoint and region, so the EC2 client's
	// config addresses all of them.
	endpoint := c.EC2Conf.Endpoint
	if endpoint == "" {
		t.Fatalf("postQueryAction: gateway endpoint not configured")
	}
	region := aws.StringValue(c.EC2Conf.Config.Region)

	form := url.Values{}
	form.Set("Action", action)
	if _, ok := params["Version"]; !ok {
		form.Set("Version", svc.version) // Query API version; bypassing the SDK.
	}
	for k, v := range params {
		form.Set(k, v)
	}
	bodyBytes := []byte(form.Encode())

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(endpoint, "/")+"/", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("postQueryAction: build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

	creds := c.EC2Conf.Config.Credentials
	if creds == nil {
		t.Fatalf("postQueryAction: no credentials on client")
	}
	signer := v4.NewSigner(creds)
	if _, err := signer.Sign(req, bytes.NewReader(bodyBytes), svc.name, region, time.Now()); err != nil {
		t.Fatalf("postQueryAction: sign: %v", err)
	}

	// Reuse the EC2 client's transport so TLS trust is consistent.
	httpClient := c.EC2Conf.Config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Transport: &http.Transport{TLSClientConfig: &tls.Config{}},
			Timeout:   env.DefaultTimeout,
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("postQueryAction: %v", err)
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("postQueryAction: read body: %v", err)
	}
	status = resp.StatusCode

	if status >= 200 && status < 300 {
		return status, body, ""
	}
	awsCode = parseAWSXMLErrorCode(body)
	return status, body, awsCode
}

// EC2 error envelopes come in two shapes; the gateway has returned both.
// ec2ErrorResponse covers <Response><Errors>, sdkErrorResponse covers <ErrorResponse>.
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
