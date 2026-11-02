//go:build e2e

package lb

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	xffModeKey  = "routing.http.xff_header_processing.mode"
	spoofedXFF  = "203.0.113.9"
	headersPath = "/__headers"
)

// headerEcho is what the target saw: one pair per header line.
type headerEcho struct {
	Peer    string      `json:"peer"`
	Headers [][2]string `json:"headers"`
}

func (e headerEcho) values(name string) []string {
	var out []string
	for _, h := range e.Headers {
		if strings.EqualFold(h[0], name) {
			out = append(out, h[1])
		}
	}
	return out
}

// echoFetcher requests the target's header echo through a load balancer with
// extra request headers ("Name: value").
type echoFetcher func(extra []string) (headerEcho, error)

func parseHeaderEcho(body []byte) (headerEcho, error) {
	var e headerEcho
	if err := json.Unmarshal(bytes.TrimSpace(body), &e); err != nil {
		return headerEcho{}, fmt.Errorf("parse header echo %q: %w", string(body), err)
	}
	return e, nil
}

// fetchEchoViaClient asks the shared probe client to request the echo through
// an internal ALB, so the caller the target sees is the client's private IP.
func fetchEchoViaClient(f *sharedFixture, lbIP string) echoFetcher {
	return func(extra []string) (headerEcho, error) {
		outfile := fmt.Sprintf("fwd-%d.txt", time.Now().UnixNano())
		body, err := json.Marshal(map[string]any{
			"proto":     "http",
			"ip":        lbIP,
			"count":     1,
			"outfile":   outfile,
			"http_port": httpPort,
			"http_path": headersPath,
			"headers":   extra,
		})
		if err != nil {
			return headerEcho{}, err
		}
		client := &http.Client{Timeout: time.Minute}
		resp, err := client.Post(fmt.Sprintf("http://%s:%d/", f.ClientPublicIP, triggerPort), "application/json", bytes.NewReader(body))
		if err != nil {
			return headerEcho{}, fmt.Errorf("trigger: %w", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return headerEcho{}, fmt.Errorf("trigger status %d", resp.StatusCode)
		}
		out, err := plainHTTPGet(fmt.Sprintf("http://%s:%d/%s", f.ClientPublicIP, httpPort, outfile), 10*time.Second)
		if err != nil {
			return headerEcho{}, err
		}
		return parseHeaderEcho([]byte(out))
	}
}

// fetchEchoHTTPS requests the echo through an HTTPS listener from the test
// runner, verifying the chain against pool.
func fetchEchoHTTPS(pubIP string, port int64, pool *x509.CertPool) echoFetcher {
	return func(extra []string) (headerEcho, error) {
		client := &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		}
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://%s:%d%s", pubIP, port, headersPath), nil)
		if err != nil {
			return headerEcho{}, err
		}
		for _, h := range extra {
			name, value, _ := strings.Cut(h, ": ")
			req.Header.Add(name, value)
		}
		resp, err := client.Do(req)
		if err != nil {
			return headerEcho{}, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return headerEcho{}, err
		}
		if resp.StatusCode != http.StatusOK {
			return headerEcho{}, fmt.Errorf("status %d (body=%q)", resp.StatusCode, string(body))
		}
		return parseHeaderEcho(body)
	}
}

// fetchEventually retries a fetch through a load balancer whose listener may
// still be coming up.
func fetchEventually(t *testing.T, fetch echoFetcher, extra ...string) headerEcho {
	t.Helper()
	var got headerEcho
	harness.EventuallyErr(t, func() error {
		e, err := fetch(extra)
		if err != nil {
			return err
		}
		got = e
		return nil
	}, 90*time.Second, 3*time.Second)
	return got
}

// assertForwardedHeaders checks the X-Forwarded-* headers the target receives:
// the caller appended to X-Forwarded-For (after a client-supplied value when
// one is sent), and Proto and Port overwritten even when the client spoofs
// them. wantCaller is the caller address when the harness knows it; otherwise
// the address must be a single valid IP that is none of avoid.
func assertForwardedHeaders(t *testing.T, label string, fetch echoFetcher, wantCaller string, avoid []string, wantProto string, wantPort int64) {
	t.Helper()
	checkCaller := func(got string) {
		t.Helper()
		if wantCaller != "" {
			assert.Equal(t, wantCaller, got, label+": caller address")
			return
		}
		assert.NotNil(t, net.ParseIP(got), label+": caller %q is not an IP", got)
		assert.NotContains(t, avoid, got, label+": caller must not be the load balancer")
	}

	plain := fetchEventually(t, fetch)
	xff := plain.values("X-Forwarded-For")
	require.Lenf(t, xff, 1, "%s: one X-Forwarded-For line, got %v", label, xff)
	checkCaller(xff[0])
	assert.Equal(t, []string{wantProto}, plain.values("X-Forwarded-Proto"), label+": proto")
	assert.Equal(t, []string{fmt.Sprint(wantPort)}, plain.values("X-Forwarded-Port"), label+": port")
	t.Logf("%s: X-Forwarded-For=%s Proto=%s Port=%d", label, xff[0], wantProto, wantPort)

	withClient := fetchEventually(t, fetch, "X-Forwarded-For: "+spoofedXFF)
	xff = withClient.values("X-Forwarded-For")
	require.Lenf(t, xff, 1, "%s: client value and caller share one line, got %v", label, xff)
	prefix, caller, ok := strings.Cut(xff[0], ", ")
	require.Truef(t, ok, "%s: want %q then the caller, got %q", label, spoofedXFF, xff[0])
	assert.Equal(t, spoofedXFF, prefix, label+": client value is kept first")
	checkCaller(caller)

	spoofed := fetchEventually(t, fetch, "X-Forwarded-Proto: spoofed", "X-Forwarded-Port: 1")
	assert.Equal(t, []string{wantProto}, spoofed.values("X-Forwarded-Proto"), label+": client proto overwritten")
	assert.Equal(t, []string{fmt.Sprint(wantPort)}, spoofed.values("X-Forwarded-Port"), label+": client port overwritten")
}

// assertRemoveMode switches the load balancer to remove, waits for the
// re-rendered config to reach the data plane, and checks a client header is
// dropped while Proto and Port stay. It then checks a bad value is rejected.
func assertRemoveMode(t *testing.T, c *harness.AWSClient, label, lbArn string, fetch echoFetcher) {
	t.Helper()
	_, err := c.ELBv2.ModifyLoadBalancerAttributes(&elbv2.ModifyLoadBalancerAttributesInput{
		LoadBalancerArn: aws.String(lbArn),
		Attributes: []*elbv2.LoadBalancerAttribute{
			{Key: aws.String(xffModeKey), Value: aws.String("remove")},
		},
	})
	require.NoErrorf(t, err, "%s: set %s=remove", label, xffModeKey)

	harness.EventuallyErr(t, func() error {
		e, ferr := fetch([]string{"X-Forwarded-For: " + spoofedXFF})
		if ferr != nil {
			return ferr
		}
		if xff := e.values("X-Forwarded-For"); len(xff) != 0 {
			return fmt.Errorf("X-Forwarded-For still reaches the target: %v", xff)
		}
		return nil
	}, 2*time.Minute, 3*time.Second)
	removed := fetchEventually(t, fetch)
	assert.NotEmpty(t, removed.values("X-Forwarded-Proto"), label+": proto stays in remove mode")
	t.Logf("%s: mode=remove drops X-Forwarded-For", label)

	_, err = c.ELBv2.ModifyLoadBalancerAttributes(&elbv2.ModifyLoadBalancerAttributesInput{
		LoadBalancerArn: aws.String(lbArn),
		Attributes: []*elbv2.LoadBalancerAttribute{
			{Key: aws.String(xffModeKey), Value: aws.String("prepend")},
		},
	})
	require.Errorf(t, err, "%s: an unknown mode must be rejected", label)
	var aerr awserr.Error
	require.Truef(t, errors.As(err, &aerr), "%s: want an AWS error, got %v", label, err)
	assert.Equal(t, "ValidationError", aerr.Code(), label+": rejected mode error code")
}

// runForwardedHeadersInternal exercises an internal ALB from the probe client.
func runForwardedHeadersInternal(t *testing.T, c *harness.AWSClient, f *sharedFixture, label, lbArn, lbIP string) {
	t.Helper()
	callerIP := privateIP(harness.InstanceENI(t, c, f.ClientID))
	require.NotEmpty(t, callerIP, "probe client needs a private IP")
	fetch := fetchEchoViaClient(f, lbIP)

	assertForwardedHeaders(t, label+" forwarding", fetch, callerIP, nil, "http", httpPort)
	assertRemoveMode(t, c, label+" forwarding", lbArn, fetch)
}

// runForwardedHeadersHTTPS exercises an internet-facing HTTPS listener from the
// test runner, whose address the load balancer sees only as some valid IP.
func runForwardedHeadersHTTPS(t *testing.T, c *harness.AWSClient, label, lbArn, pubIP string, port int64, pool *x509.CertPool, lbIPs []string) {
	t.Helper()
	fetch := fetchEchoHTTPS(pubIP, port, pool)
	assertForwardedHeaders(t, label+" forwarding", fetch, "", lbIPs, "https", port)
	assertRemoveMode(t, c, label+" forwarding", lbArn, fetch)
}
