//go:build e2e

package lb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// healthIntervalSeconds and healthThreshold keep detection near 10s; the
	// routing bound adds two lb-agent ticks and a config reload on top.
	healthIntervalSeconds = 5
	healthThreshold       = 2
	healthRoutingBound    = 2 * time.Minute
	healthProbeCount      = 20
)

// runNLBUnhealthyTargetsSuite checks that an NLB stops sending traffic to a
// target whose health check fails while it still accepts TCP, sends traffic
// again once it recovers, and fails open when every target is unhealthy.
func runNLBUnhealthyTargetsSuite(t *testing.T, c *harness.AWSClient, f *sharedFixture) {
	t.Helper()
	const label = "NLB unhealthy targets"

	tgArn := createHealthCheckedTCPTargetGroup(t, c, f, "lb-e2e-nlb-health-tg")
	t.Cleanup(func() { deleteTargetGroup(t, c, tgArn) })
	registerTargets(t, c, tgArn, f.AppInstanceIDs)
	t.Cleanup(func() { deregisterTargets(t, c, tgArn, f.AppInstanceIDs) })

	lb, _ := createActiveLB(t, c, f, "lb-e2e-nlb-health", "network", "internal", "TCP", tcpPort, tgArn, label, nil)
	captureLBConsoleOnFailure(t, c, "net", lb)
	harness.WaitForTargetsHealthy(t, c, tgArn, 2, label, 2*time.Minute)

	lbIP := privateIP(lbENI(t, c, "net", lb))
	require.NotEmpty(t, lbIP, label+" needs private IP")

	appIPs := make([]string, len(f.AppInstanceIDs))
	responders := make([]string, len(f.AppInstanceIDs))
	for i, id := range f.AppInstanceIDs {
		appIPs[i] = privateIP(harness.InstanceENI(t, c, id))
		require.NotEmptyf(t, appIPs[i], "app instance %s needs a private IP", id)
		responders[i] = requireSingleTCPResponder(t, f, appIPs[i])
	}
	require.NotEqual(t, responders[0], responders[1], "the two app instances must answer differently")
	t.Cleanup(func() {
		for _, ip := range appIPs {
			_, _ = probeTrigger(f, "http", ip, healthPort, "/ok", 1)
		}
	})

	harness.Phase(t, "%s: both healthy, both receive traffic", label)
	r := requireTCPDistribution(t, f, lbIP, healthRoutingBound, func(r harness.TrafficResult) bool {
		return len(r.Distribution) == 2 && r.Successful == healthProbeCount
	}, "both targets serve")
	t.Logf("%s: distribution %v", label, r.Distribution)

	harness.Phase(t, "%s: target %s fails its health check but still accepts TCP", label, f.AppInstanceIDs[0])
	setHealth(t, f, appIPs[0], false)
	waitTargetState(t, c, tgArn, f.AppInstanceIDs[0], "unhealthy", healthRoutingBound)
	r = requireTCPDistribution(t, f, lbIP, healthRoutingBound, func(r harness.TrafficResult) bool {
		return r.Successful == healthProbeCount && r.Distribution[responders[0]] == 0
	}, "no request reaches the unhealthy target")
	assert.Positive(t, r.Distribution[responders[1]], "the healthy target serves all traffic")
	// Still accepting TCP directly proves it was removed by health, not by being down.
	assert.Equal(t, responders[0], requireSingleTCPResponder(t, f, appIPs[0]),
		"the unhealthy target keeps accepting TCP")

	harness.Phase(t, "%s: target recovers and receives traffic again", label)
	setHealth(t, f, appIPs[0], true)
	waitTargetState(t, c, tgArn, f.AppInstanceIDs[0], "healthy", healthRoutingBound)
	requireTCPDistribution(t, f, lbIP, healthRoutingBound, func(r harness.TrafficResult) bool {
		return len(r.Distribution) == 2 && r.Successful == healthProbeCount
	}, "recovered target serves again")

	harness.Phase(t, "%s: every target unhealthy, load balancer fails open", label)
	for _, ip := range appIPs {
		setHealth(t, f, ip, false)
	}
	for _, id := range f.AppInstanceIDs {
		waitTargetState(t, c, tgArn, id, "unhealthy", healthRoutingBound)
	}
	requireTCPDistribution(t, f, lbIP, healthRoutingBound, func(r harness.TrafficResult) bool {
		return len(r.Distribution) == 2 && r.Successful == healthProbeCount
	}, "fail open sends traffic to every target")
}

// runALBFailOpenSuite checks that an ALB whose targets are all failing their
// health check still serves traffic from them instead of answering 503.
func runALBFailOpenSuite(t *testing.T, c *harness.AWSClient, f *sharedFixture) {
	t.Helper()
	const label = "ALB fail open"

	tgArn := createTargetGroup(t, c, f, "lb-e2e-alb-failopen-tg", "HTTP", httpPort, "/hc.html")
	t.Cleanup(func() { deleteTargetGroup(t, c, tgArn) })
	registerTargets(t, c, tgArn, f.AppInstanceIDs)
	t.Cleanup(func() { deregisterTargets(t, c, tgArn, f.AppInstanceIDs) })

	sgID := createScenarioSG(t, c, f, "lb-e2e-alb-failopen-sg", httpPort)
	lb, _ := createActiveLB(t, c, f, "lb-e2e-alb-failopen", "application", "internal", "HTTP", httpPort, tgArn, label, []string{sgID})
	harness.WaitForTargetsHealthy(t, c, tgArn, 2, label, 2*time.Minute)

	lbIP := privateIP(lbENI(t, c, "app", lb))
	require.NotEmpty(t, lbIP, label+" needs private IP")

	harness.Phase(t, "%s: a health-check path change reaches the load balancer", label)
	setHealthPath(t, c, tgArn, "/health-check-missing")
	for _, id := range f.AppInstanceIDs {
		waitTargetState(t, c, tgArn, id, "unhealthy", healthRoutingBound)
	}
	setHealthPath(t, c, tgArn, "/hc.html")
	harness.WaitForTargetsHealthy(t, c, tgArn, 2, label, healthRoutingBound)

	appIPs := make([]string, len(f.AppInstanceIDs))
	for i, id := range f.AppInstanceIDs {
		appIPs[i] = privateIP(harness.InstanceENI(t, c, id))
		require.NotEmptyf(t, appIPs[i], "app instance %s needs a private IP", id)
	}
	t.Cleanup(func() {
		for _, ip := range appIPs {
			_, _ = probeTrigger(f, "http", ip, healthPort, "/ok", 1)
		}
	})

	harness.Phase(t, "%s: health path stops answering 200 on every target", label)
	for _, ip := range appIPs {
		setHealth(t, f, ip, false)
	}
	for _, id := range f.AppInstanceIDs {
		waitTargetState(t, c, tgArn, id, "unhealthy", healthRoutingBound)
	}

	harness.EventuallyErr(t, func() error {
		r, err := probeTrigger(f, "http", lbIP, httpPort, "/index.html", healthProbeCount)
		if err != nil {
			return err
		}
		if r.Successful != healthProbeCount {
			return fmt.Errorf("%d/%d requests served while every target is unhealthy", r.Successful, healthProbeCount)
		}
		return nil
	}, healthRoutingBound, 5*time.Second)
}

// createHealthCheckedTCPTargetGroup creates a TCP target group whose health
// check is an HTTP GET to the toggle endpoint, separate from the data port.
func createHealthCheckedTCPTargetGroup(t *testing.T, c *harness.AWSClient, f *sharedFixture, name string) string {
	t.Helper()
	// e2e:allow-create — the health-check settings are the subject under test.
	out, err := c.ELBv2.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:                       aws.String(name),
		Protocol:                   aws.String("TCP"),
		Port:                       aws.Int64(tcpPort),
		VpcId:                      aws.String(f.VPCID),
		HealthCheckProtocol:        aws.String("HTTP"),
		HealthCheckPort:            aws.String(fmt.Sprint(healthPort)),
		HealthCheckPath:            aws.String("/health"),
		HealthCheckIntervalSeconds: aws.Int64(healthIntervalSeconds),
		HealthyThresholdCount:      aws.Int64(healthThreshold),
		UnhealthyThresholdCount:    aws.Int64(healthThreshold),
	})
	require.NoErrorf(t, err, "create-target-group %s", name)
	arn := aws.StringValue(out.TargetGroups[0].TargetGroupArn)
	t.Logf("TG %s: %s", name, arn)
	return arn
}

// setHealth flips an app instance's health endpoint through the probe client.
func setHealth(t *testing.T, f *sharedFixture, appIP string, healthy bool) {
	t.Helper()
	path := "/fail"
	if healthy {
		path = "/ok"
	}
	r, err := probeTrigger(f, "http", appIP, healthPort, path, 1)
	require.NoErrorf(t, err, "toggle health %s on %s", path, appIP)
	t.Logf("health %s on %s: %d responses", path, appIP, r.Total)
}

// setHealthPath changes a target group's health-check path.
func setHealthPath(t *testing.T, c *harness.AWSClient, tgArn, path string) {
	t.Helper()
	_, err := c.ELBv2.ModifyTargetGroup(&elbv2.ModifyTargetGroupInput{
		TargetGroupArn:  aws.String(tgArn),
		HealthCheckPath: aws.String(path),
	})
	require.NoErrorf(t, err, "modify-target-group health path %s", path)
}

// waitTargetState polls DescribeTargetHealth until one target reports want.
func waitTargetState(t *testing.T, c *harness.AWSClient, tgArn, targetID, want string, timeout time.Duration) {
	t.Helper()
	start := time.Now()
	defer func() { t.Logf("target %s reached %s after %s", targetID, want, time.Since(start).Round(time.Second)) }()
	harness.EventuallyErr(t, func() error {
		out, err := c.ELBv2.DescribeTargetHealth(&elbv2.DescribeTargetHealthInput{TargetGroupArn: aws.String(tgArn)})
		if err != nil {
			return err
		}
		for _, th := range out.TargetHealthDescriptions {
			if aws.StringValue(th.Target.Id) == targetID {
				got := aws.StringValue(th.TargetHealth.State)
				if got == want {
					return nil
				}
				return fmt.Errorf("target %s is %s, want %s", targetID, got, want)
			}
		}
		return fmt.Errorf("target %s not listed", targetID)
	}, timeout, 3*time.Second)
}

// requireSingleTCPResponder returns the hostname one backend answers with on
// the TCP echo port, probing it directly rather than through a load balancer.
func requireSingleTCPResponder(t *testing.T, f *sharedFixture, appIP string) string {
	t.Helper()
	var name string
	harness.EventuallyErr(t, func() error {
		r, err := probeTrigger(f, "tcp", appIP, tcpPort, "", 1)
		if err != nil {
			return err
		}
		if len(r.Distribution) != 1 {
			return fmt.Errorf("no answer from %s:%d", appIP, tcpPort)
		}
		for k := range r.Distribution {
			name = k
		}
		return nil
	}, time.Minute, 3*time.Second)
	return name
}

// requireTCPDistribution probes the load balancer until ok accepts a run.
func requireTCPDistribution(t *testing.T, f *sharedFixture, lbIP string, timeout time.Duration, ok func(harness.TrafficResult) bool, what string) harness.TrafficResult {
	t.Helper()
	var last harness.TrafficResult
	start := time.Now()
	defer func() { t.Logf("%s after %s", what, time.Since(start).Round(time.Second)) }()
	harness.EventuallyErr(t, func() error {
		r, err := probeTrigger(f, "tcp", lbIP, tcpPort, "", healthProbeCount)
		if err != nil {
			return err
		}
		last = r
		if !ok(r) {
			return fmt.Errorf("%s: distribution %v, %d/%d answered", what, r.Distribution, r.Successful, r.Total)
		}
		return nil
	}, timeout, 5*time.Second)
	return last
}

// probeTrigger asks the shared probe client to send count requests to ip and
// returns what each one saw. proto "http" uses port as the HTTP port; "tcp"
// uses it as the echo port.
func probeTrigger(f *sharedFixture, proto, ip string, port int64, path string, count int) (harness.TrafficResult, error) {
	resultsFile := fmt.Sprintf("hlt-%s-%d-%d.txt", proto, port, time.Now().UnixNano())
	order := map[string]any{
		"proto":     proto,
		"ip":        ip,
		"count":     count,
		"outfile":   resultsFile,
		"http_port": port,
		"http_path": path,
		"tcp_port":  port,
	}
	body, err := json.Marshal(order)
	if err != nil {
		return harness.TrafficResult{}, err
	}
	client := &http.Client{Timeout: 3 * time.Minute}
	resp, err := client.Post(fmt.Sprintf("http://%s:%d/", f.ClientPublicIP, triggerPort), "application/json", bytes.NewReader(body))
	if err != nil {
		return harness.TrafficResult{}, fmt.Errorf("trigger: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return harness.TrafficResult{}, fmt.Errorf("trigger status %d", resp.StatusCode)
	}
	results, err := plainHTTPGet(fmt.Sprintf("http://%s:%d/%s", f.ClientPublicIP, httpPort, resultsFile), 10*time.Second)
	if err != nil {
		return harness.TrafficResult{}, fmt.Errorf("fetch %s: %w", resultsFile, err)
	}
	return harness.VerifyResultsLines(results, proto), nil
}
