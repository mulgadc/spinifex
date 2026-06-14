//go:build e2e

// Package reboot is the single-node host-reboot resilience suite. It MUST run outside
// the cluster (on the CI runner) because systemctl reboot kills any in-VM process.
// AWS API traffic crosses the WAN via the SDK; SSH ops use harness.PeerSSH.
package reboot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	keyName       = "reboot-e2e-key"
	tgName        = "reboot-e2e-tg"
	albName       = "reboot-e2e-alb"
	sgName        = "reboot-e2e-sg"
	vpcCIDR       = "10.210.0.0/16"
	subnetCIDR    = "10.210.1.0/24"
	httpPort      = 80
	probesPerRun  = 20
	appHTTPUnit   = "reboot-e2e-httpd.service"
	rebootDoctype = "reboot-e2e"
)

// appUserData provisions a systemd-managed HTTP responder. Using a unit (not nohup) ensures
// the responder survives the guest's own restart after host reboot.
const appUserData = `#!/bin/bash
set -e
INSTANCE_ID=$(hostname)
install -d -m 0755 /var/lib/httpd
echo "{\"instance_id\": \"${INSTANCE_ID}\"}" > /var/lib/httpd/index.html
cat > /etc/systemd/system/reboot-e2e-httpd.service <<'UNIT'
[Unit]
Description=Reboot E2E HTTP responder
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/bin/python3 -m http.server 80 --bind 0.0.0.0 --directory /var/lib/httpd
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now reboot-e2e-httpd.service

# Workaround for viperblock WAL <4MB skip-upload bug:
# WriteWALToChunk(false) is a no-op when the WAL is under 4MB so small
# user-data writes (the unit file, index.html) sit in the on-host WAL
# and are lost when the host reboots before the WAL hits the threshold.
# Write 8MB of zeros + sync to push WAL over the threshold so the
# unit-file write definitely lands in predastore before the host reboot.
# Drop when upstream fix lands.
dd if=/dev/zero of=/var/lib/spinifex-e2e-pad bs=1M count=8 status=none
sync
rm -f /var/lib/spinifex-e2e-pad
sync
`

type fixture struct {
	env       *harness.Env
	artifacts string
	ssh       *harness.PeerSSH
	aws       *harness.AWSClient

	instanceType string
	amiID        string

	vpcID    string
	subnetID string
	igwID    string
	sgID     string

	appInstanceIDs []string
	preIPs         map[string]string
	privateKeyPath string // local PEM written from CreateKeyPair KeyMaterial

	tgArn       string
	albArn      string
	albID       string
	listenerArn string
	albPublicIP string
	albEniMAC   string

	preSwitches int
	prePorts    int

	rebootStart time.Time
	rebootDone  bool
	mu          sync.Mutex // guards rebootDone for the on-failure dump

	timeouts timeouts
}

type timeouts struct {
	rebootWait      time.Duration
	daemonReady     time.Duration
	instanceRunning time.Duration
	lbRecover       time.Duration
}

// TestRebootResilience runs the 8-phase reboot resilience sequence.
// Phases are sequential: the reboot is a global event and each phase depends on the prior.
func TestRebootResilience(t *testing.T) {
	env := harness.LoadEnv(t)
	requireRunnerMode(t, env)

	fix := &fixture{
		env:       env,
		artifacts: harness.ArtifactDir(t, env),
		ssh:       harness.NewPeerSSH(),
		preIPs:    map[string]string{},
		timeouts: timeouts{
			rebootWait:      durationEnv("REBOOT_WAIT_SECS", 300*time.Second),
			daemonReady:     durationEnv("DAEMON_READY_SECS", 180*time.Second),
			instanceRunning: durationEnv("INSTANCE_RUNNING_SECS", 120*time.Second),
			// 5min budget: post-reboot the daemon relaunches app VMs and the ALB sys.micro
			// simultaneously; HC probing starts immediately but targets honestly report
			// unhealthy until the responder unit finishes starting (no cloud-init shortcut).
			lbRecover: durationEnv("LB_RECOVER_SECS", 300*time.Second),
		},
	}

	resolveTrust(t, fix)
	fix.aws = harness.NewAWSClient(t, env)

	t.Cleanup(func() { cleanup(t, fix) })

	harness.OnFailure(t, func() {
		// Dump diagnostics windowed from reboot onwards (if reboot fired) so
		// the analysis surfaces journal lines from the recovery path rather
		// than the per-instance state-transition spam during setup.
		fix.mu.Lock()
		since := fix.rebootStart
		done := fix.rebootDone
		fix.mu.Unlock()
		if !done {
			since = time.Time{}
		}
		dumpDiagnostics(t, fix, since)
		dumpAppDiagnostics(t, fix)
	})

	harness.Phase(t, "Phase 0 — Prerequisites")
	phase0Prereqs(t, fix)

	harness.Phase(t, "Phase 1 — VPC + Subnet + Security Group")
	phase1Network(t, fix)

	harness.Phase(t, "Phase 2 — App instances")
	phase2Instances(t, fix)

	harness.Phase(t, "Phase 3 — ALB + target group + listener")
	phase3LB(t, fix)

	harness.Phase(t, "Phase 4 — Pre-reboot traffic")
	runHTTPBurst(t, fix, "ALB pre-reboot")

	harness.Phase(t, "Phase 5 — Snapshot pre-reboot state")
	phase5Snapshot(t, fix)

	harness.Phase(t, "Phase 6 — systemctl reboot")
	phase6Reboot(t, fix)

	harness.Phase(t, "Phase 7 — Wait for spinifex readiness")
	phase7Readiness(t, fix)

	harness.Phase(t, "Phase 8 — Post-reboot assertions")
	phase8Asserts(t, fix)
}

// --- Setup helpers -------------------------------------------------------

// requireRunnerMode bails early if the env isn't runner-resident (SPINIFEX_WAN_IP or endpoint missing).
func requireRunnerMode(t *testing.T, env *harness.Env) {
	t.Helper()
	if env.WANHost == "" {
		t.Fatalf("reboot suite requires SPINIFEX_WAN_IP (no WAN host resolved)")
	}
	if env.Mode != harness.ModeSingle {
		t.Skipf("reboot suite is single-node only (mode=%s)", env.Mode)
	}
	if endpoint := os.Getenv("SPINIFEX_AWS_ENDPOINT"); endpoint == "" {
		t.Fatalf("reboot suite requires SPINIFEX_AWS_ENDPOINT=https://%s:%d", env.WANHost, env.AWSGWPort)
	}
}

// resolveTrust SCPs the spinifex CA to a tmp file for real TLS verification.
// Falls back to SPINIFEX_AWS_INSECURE=1 on any error; CA trust is the cert suite's concern.
func resolveTrust(t *testing.T, fix *fixture) {
	t.Helper()
	if os.Getenv("SPINIFEX_AWS_INSECURE") == "1" {
		t.Logf("trust: SPINIFEX_AWS_INSECURE=1 set; skipping CA fetch")
		return
	}
	if os.Getenv("SPINIFEX_CA_CERT") != "" {
		t.Logf("trust: SPINIFEX_CA_CERT already set; skipping CA fetch")
		return
	}

	tmpCA := filepath.Join(fix.artifacts, "spinifex-ca.pem")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidates := []string{
		"/etc/spinifex/ca.pem",
		"$HOME/spinifex/config/ca.pem",
	}
	var lastErr error
	for _, remote := range candidates {
		out, err := fix.ssh.Run(ctx, fix.env.WANHost, "cat "+remote)
		if err == nil && len(out) > 0 {
			if writeErr := os.WriteFile(tmpCA, out, 0o600); writeErr == nil {
				t.Logf("trust: fetched CA from %s -> %s", remote, tmpCA)
				t.Setenv("SPINIFEX_CA_CERT", tmpCA)
				return
			} else {
				lastErr = writeErr
			}
		} else {
			lastErr = err
		}
	}
	t.Logf("trust: CA fetch failed (%v); falling back to InsecureSkipVerify", lastErr)
	t.Setenv("SPINIFEX_AWS_INSECURE", "1")
}

// --- Phases --------------------------------------------------------------

func phase0Prereqs(t *testing.T, fix *fixture) {
	t.Helper()
	harness.Step(t, "WAN_IP=%s", fix.env.WANHost)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	require.NoError(t, fix.ssh.Ping(ctx, fix.env.WANHost), "ssh to %s", fix.env.WANHost)
	harness.Detail(t, "ssh", "ok")

	fix.instanceType = discoverNanoInstanceType(t, fix.aws)
	harness.Detail(t, "instance_type", fix.instanceType)

	fix.amiID = discoverAMI(t, fix.aws)
	harness.Detail(t, "ami", fix.amiID)

	// Capture KeyMaterial so post-reboot diagnostics can SSH into guest VMs.
	_, _ = fix.aws.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(keyName)})
	kpOut, err := fix.aws.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(keyName)})
	require.NoErrorf(t, err, "create-key-pair %s", keyName)
	material := aws.StringValue(kpOut.KeyMaterial)
	require.NotEmptyf(t, material, "create-key-pair %s returned no KeyMaterial", keyName)
	fix.privateKeyPath = filepath.Join(fix.artifacts, keyName+".pem")
	require.NoError(t, os.WriteFile(fix.privateKeyPath, []byte(material), 0o600), "write private key")
	harness.Detail(t, "key_pair", keyName)
}

func phase1Network(t *testing.T, fix *fixture) {
	t.Helper()

	vpcOut, err := fix.aws.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String(vpcCIDR)})
	require.NoError(t, err, "create-vpc")
	fix.vpcID = aws.StringValue(vpcOut.Vpc.VpcId)
	harness.Detail(t, "vpc", fix.vpcID)

	igwOut, err := fix.aws.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
	require.NoError(t, err, "create-internet-gateway")
	fix.igwID = aws.StringValue(igwOut.InternetGateway.InternetGatewayId)
	_, err = fix.aws.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(fix.igwID),
		VpcId:             aws.String(fix.vpcID),
	})
	require.NoError(t, err, "attach-internet-gateway")
	harness.Detail(t, "igw", fix.igwID)

	subnetOut, err := fix.aws.EC2.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(fix.vpcID),
		CidrBlock: aws.String(subnetCIDR),
	})
	require.NoError(t, err, "create-subnet")
	fix.subnetID = aws.StringValue(subnetOut.Subnet.SubnetId)
	_, _ = fix.aws.EC2.ModifySubnetAttribute(&ec2.ModifySubnetAttributeInput{
		SubnetId:            aws.String(fix.subnetID),
		MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	harness.Detail(t, "subnet", fix.subnetID)

	sgOut, err := fix.aws.EC2.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(sgName),
		Description: aws.String("Reboot E2E shared SG (ALB + app instances)"),
		VpcId:       aws.String(fix.vpcID),
	})
	require.NoError(t, err, "create-security-group")
	fix.sgID = aws.StringValue(sgOut.GroupId)

	// Use IpPermissions form — vpcd ignores the top-level shorthand field.
	// tcp/22 is opened so on-failure diagnostics can SSH into app guests.
	_, err = fix.aws.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(fix.sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(httpPort),
			ToPort:     aws.Int64(httpPort),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
		}, {
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(22),
			ToPort:     aws.Int64(22),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
		}},
	})
	require.NoError(t, err, "authorize tcp/80 on %s", fix.sgID)
	harness.Detail(t, "sg", fix.sgID)
}

func phase2Instances(t *testing.T, fix *fixture) {
	t.Helper()
	for i := 0; i < 2; i++ {
		out, err := fix.aws.EC2.RunInstances(&ec2.RunInstancesInput{
			ImageId:          aws.String(fix.amiID),
			InstanceType:     aws.String(fix.instanceType),
			KeyName:          aws.String(keyName),
			SubnetId:         aws.String(fix.subnetID),
			SecurityGroupIds: []*string{aws.String(fix.sgID)},
			UserData:         aws.String(base64.StdEncoding.EncodeToString([]byte(appUserData))),
			MinCount:         aws.Int64(1),
			MaxCount:         aws.Int64(1),
		})
		require.NoErrorf(t, err, "run-instances app%d", i+1)
		require.NotEmpty(t, out.Instances)
		id := aws.StringValue(out.Instances[0].InstanceId)
		fix.appInstanceIDs = append(fix.appInstanceIDs, id)
		harness.Detail(t, fmt.Sprintf("app%d", i+1), id)
	}

	for _, id := range fix.appInstanceIDs {
		harness.WaitForInstanceRunning(t, fix.aws, id, fix.timeouts.instanceRunning)
	}

	for _, id := range fix.appInstanceIDs {
		ip := describePrivateIP(t, fix.aws, id)
		require.NotEmptyf(t, ip, "%s has no PrivateIpAddress", id)
		fix.preIPs[id] = ip
		harness.Detail(t, id, ip)
	}
}

func phase3LB(t *testing.T, fix *fixture) {
	t.Helper()

	tgOut, err := fix.aws.ELBv2.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:                       aws.String(tgName),
		Protocol:                   aws.String("HTTP"),
		Port:                       aws.Int64(httpPort),
		VpcId:                      aws.String(fix.vpcID),
		HealthCheckPath:            aws.String("/index.html"),
		HealthCheckIntervalSeconds: aws.Int64(5),
		HealthyThresholdCount:      aws.Int64(2),
		UnhealthyThresholdCount:    aws.Int64(2),
	})
	require.NoError(t, err, "create-target-group")
	fix.tgArn = aws.StringValue(tgOut.TargetGroups[0].TargetGroupArn)
	harness.Detail(t, "tg", fix.tgArn)

	targets := make([]*elbv2.TargetDescription, len(fix.appInstanceIDs))
	for i, id := range fix.appInstanceIDs {
		targets[i] = &elbv2.TargetDescription{Id: aws.String(id)}
	}
	_, err = fix.aws.ELBv2.RegisterTargets(&elbv2.RegisterTargetsInput{
		TargetGroupArn: aws.String(fix.tgArn),
		Targets:        targets,
	})
	require.NoError(t, err, "register-targets")

	lbOut, err := fix.aws.ELBv2.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:           aws.String(albName),
		Scheme:         aws.String("internet-facing"),
		Subnets:        []*string{aws.String(fix.subnetID)},
		SecurityGroups: []*string{aws.String(fix.sgID)},
	})
	require.NoError(t, err, "create-load-balancer")
	require.NotEmpty(t, lbOut.LoadBalancers)
	fix.albArn = aws.StringValue(lbOut.LoadBalancers[0].LoadBalancerArn)
	parts := strings.Split(fix.albArn, "/")
	fix.albID = parts[len(parts)-1]
	harness.Detail(t, "alb", fix.albArn)

	listenerOut, err := fix.aws.ELBv2.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(fix.albArn),
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(httpPort),
		DefaultActions: []*elbv2.Action{{
			Type:           aws.String("forward"),
			TargetGroupArn: aws.String(fix.tgArn),
		}},
	})
	require.NoError(t, err, "create-listener")
	fix.listenerArn = aws.StringValue(listenerOut.Listeners[0].ListenerArn)

	harness.WaitForLBActive(t, fix.aws, fix.albArn, "ALB", 5*time.Minute)

	eniDesc := fmt.Sprintf("ELB app/%s/%s", albName, fix.albID)
	var eni *ec2.NetworkInterface
	harness.EventuallyErr(t, func() error {
		out, err := fix.aws.EC2.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
			Filters: []*ec2.Filter{{
				Name:   aws.String("description"),
				Values: []*string{aws.String(eniDesc)},
			}},
		})
		if err != nil {
			return err
		}
		if len(out.NetworkInterfaces) == 0 {
			return fmt.Errorf("no ENI for %s", eniDesc)
		}
		eni = out.NetworkInterfaces[0]
		return nil
	}, 30*time.Second, 2*time.Second)

	if eni.Association != nil {
		fix.albPublicIP = aws.StringValue(eni.Association.PublicIp)
	}
	fix.albEniMAC = aws.StringValue(eni.MacAddress)
	require.NotEmpty(t, fix.albPublicIP, "ALB has no public IP")
	harness.Detail(t, "alb_public_ip", fix.albPublicIP, "alb_mac", fix.albEniMAC)

	harness.WaitForTargetsHealthy(t, fix.aws, fix.tgArn, 2, "ALB", 3*time.Minute)
}

func phase5Snapshot(t *testing.T, fix *fixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := fix.ssh.Run(ctx, fix.env.WANHost, "sudo ovn-nbctl show 2>/dev/null || true")
	if err != nil {
		t.Logf("ovn-nbctl pre-snapshot: %v (continuing with zero counts)", err)
		return
	}
	fix.preSwitches, fix.prePorts = countOVN(string(out))
	harness.Detail(t, "pre_switches", fix.preSwitches, "pre_ports", fix.prePorts)
	harness.DumpFile(t, fix.artifacts, "ovn-pre.txt", out)

	// Capture OVN chassis identity pre-reboot so the post-reboot dump can be diffed;
	// SB Chassis _uuid churn across reboot orphans every Port_Binding claim.
	cstate, err := fix.ssh.Run(ctx, fix.env.WANHost, ovnChassisStateBundle)
	if err != nil {
		t.Logf("ovn chassis pre-snapshot: %v (non-fatal)", err)
	}
	harness.DumpFile(t, fix.artifacts, "ovn-pre-chassis.txt", cstate)
}

func phase6Reboot(t *testing.T, fix *fixture) {
	t.Helper()
	fix.mu.Lock()
	fix.rebootStart = time.Now()
	fix.mu.Unlock()
	harness.Step(t, "issuing reboot via ssh (connection will drop)")

	// SSH dies mid-command when the host shuts down; ignore the error.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = fix.ssh.Run(ctx, fix.env.WANHost, "sudo systemctl reboot")

	// Wait for shutdown to begin so the pre-reboot SSH window isn't mistaken for "back up".
	time.Sleep(5 * time.Second)

	harness.Step(t, "polling TCP/22 (timeout %s)", fix.timeouts.rebootWait)
	deadline := time.Now().Add(fix.timeouts.rebootWait)
	var lastErr error
	for {
		if time.Now().After(deadline) {
			t.Fatalf("host SSH did not come back within %s (last err: %v)", fix.timeouts.rebootWait, lastErr)
		}
		conn, dialErr := net.DialTimeout("tcp", net.JoinHostPort(fix.env.WANHost, "22"), 5*time.Second)
		if dialErr == nil {
			_ = conn.Close()
			pingCtx, pingCancel := context.WithTimeout(context.Background(), 15*time.Second)
			err := fix.ssh.Ping(pingCtx, fix.env.WANHost)
			pingCancel()
			if err == nil {
				break
			}
			lastErr = err
		} else {
			lastErr = dialErr
		}
		time.Sleep(5 * time.Second)
	}

	fix.mu.Lock()
	fix.rebootDone = true
	elapsed := time.Since(fix.rebootStart).Round(time.Second)
	fix.mu.Unlock()
	harness.Detail(t, "ssh_back_after", elapsed)
}

func phase7Readiness(t *testing.T, fix *fixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, svc := range serviceList {
		out, err := fix.ssh.Run(ctx, fix.env.WANHost, "systemctl is-active "+svc)
		state := strings.TrimSpace(string(out))
		if err != nil && state == "" {
			state = "n/a"
		}
		harness.Detail(t, svc, state)
	}

	harness.Step(t, "polling describe-instance-types (timeout %s)", fix.timeouts.daemonReady)
	harness.EventuallyErr(t, func() error {
		_, err := fix.aws.EC2.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{})
		return err
	}, fix.timeouts.daemonReady, 5*time.Second)
	harness.Detail(t, "gateway", "responding")
}

func phase8Asserts(t *testing.T, fix *fixture) {
	t.Helper()

	// 8.1 instances back to running
	harness.Step(t, "polling instance state (timeout %s)", fix.timeouts.instanceRunning)
	harness.EventuallyErr(t, func() error {
		for _, id := range fix.appInstanceIDs {
			state := describeInstanceState(t, fix.aws, id)
			if state != "running" {
				return fmt.Errorf("%s state=%s", id, state)
			}
		}
		return nil
	}, fix.timeouts.instanceRunning, 5*time.Second)
	harness.Detail(t, "instances", "all running")

	// 8.2 private IPs preserved
	for _, id := range fix.appInstanceIDs {
		post := describePrivateIP(t, fix.aws, id)
		assert.Equalf(t, fix.preIPs[id], post, "%s IP drift", id)
	}

	// 8.3 LaunchTime > REBOOT_START (relaunch signal)
	for _, id := range fix.appInstanceIDs {
		lt := describeLaunchTime(t, fix.aws, id)
		require.Falsef(t, lt.IsZero(), "%s missing LaunchTime", id)
		assert.Truef(t, !lt.Before(fix.rebootStart),
			"%s LaunchTime=%s predates reboot=%s", id, lt, fix.rebootStart)
	}

	// 8.4 ALB active
	harness.WaitForLBActive(t, fix.aws, fix.albArn, "ALB post-reboot", fix.timeouts.lbRecover)

	// 8.5 targets healthy — capture diagnostics before failing so a timeout can be
	// pinned to network-level drop vs. app/gateway stall.
	if !pollTargetsHealthy(fix.aws, fix.tgArn, 2, fix.timeouts.lbRecover) {
		harness.Step(t, "ALB post-reboot: targets NOT healthy — capturing diagnostics")
		dctx, dcancel := context.WithTimeout(context.Background(), 120*time.Second)
		dumpHeartbeatDiagnostics(dctx, t, fix)
		dumpOVNDataplane(dctx, t, fix)
		dumpOVNClaimState(dctx, t, fix)
		dumpAppDiagnostics(t, fix)
		dcancel()
		t.Fatalf("ALB post-reboot: targets not healthy within %s (see diag-heartbeat.txt / diag-ovn-flows.txt / diag-ovn-claim.txt / diag-host-net.txt)", fix.timeouts.lbRecover)
	}
	harness.Step(t, "ALB post-reboot: 2 targets healthy")

	// 8.6 Probe until both backends' instance_ids are seen before asserting round-robin:
	// healthy-cache state may show stale "healthy" until both responders are actually up.
	harness.Step(t, "waiting for ALB to serve HTTP from both backends")
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer probeCancel()
	seen := map[string]bool{}
	served := false
	serveDeadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(serveDeadline) {
		out, err := fix.ssh.Run(probeCtx, fix.env.WANHost,
			fmt.Sprintf("curl -s --connect-timeout 2 --max-time 3 'http://%s:%d/' 2>/dev/null", fix.albPublicIP, httpPort))
		if err == nil {
			if id := instanceIDFromResponse(out); id != "" {
				seen[id] = true
			}
		}
		if len(seen) >= 2 {
			served = true
			break
		}
		time.Sleep(1 * time.Second)
	}
	// 8.5 reports targets healthy via the daemon's own view, which can be green
	// while the north-south EIP datapath is dark. Capture dataplane diagnostics on
	// a serve failure too — otherwise this path leaves no flow/ARP evidence.
	if !served {
		harness.Step(t, "ALB serve FAILED — capturing datapath diagnostics")
		dctx, dcancel := context.WithTimeout(context.Background(), 120*time.Second)
		dumpALBProbe(dctx, t, fix)
		dumpOVNDataplane(dctx, t, fix)
		dumpOVNClaimState(dctx, t, fix)
		dumpHeartbeatDiagnostics(dctx, t, fix)
		dumpAppDiagnostics(t, fix)
		dcancel()
		t.Fatalf("ALB did not serve from both backends within 180s post-reboot (saw=%v) "+
			"— see diag-ovn-flows.txt / diag-alb-probe.txt / diag-heartbeat.txt", seen)
	}

	runHTTPBurst(t, fix, "ALB post-reboot")

	// 8.6.5 Always capture in-guest responder state (pass or fail) to confirm genuine liveness.
	dumpAppDiagnostics(t, fix)

	// 8.7 ovn-nbctl drift check
	dumpCtx, dumpCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dumpCancel()
	out, err := fix.ssh.Run(dumpCtx, fix.env.WANHost, "sudo ovn-nbctl show 2>/dev/null || true")
	if err != nil {
		t.Logf("ovn-nbctl post-snapshot: %v (skipping drift assert)", err)
		return
	}
	harness.DumpFile(t, fix.artifacts, "ovn-post.txt", out)
	postSwitches, postPorts := countOVN(string(out))
	harness.Detail(t, "post_switches", postSwitches, "post_ports", postPorts)
	assert.Equalf(t, fix.preSwitches, postSwitches,
		"ovn switch count drift: pre=%d post=%d", fix.preSwitches, postSwitches)
	assert.Equalf(t, fix.prePorts, postPorts,
		"ovn port count drift: pre=%d post=%d", fix.prePorts, postPorts)
}

// --- HTTP burst ----------------------------------------------------------

func runHTTPBurst(t *testing.T, fix *fixture, label string) {
	t.Helper()
	harness.Step(t, "sending %d HTTP requests (%s)", probesPerRun, label)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := fmt.Sprintf(
		"for i in $(seq 1 %d); do curl -s --max-time 5 'http://%s:%d/' 2>/dev/null; echo; done",
		probesPerRun, fix.albPublicIP, httpPort,
	)
	out, err := fix.ssh.Run(ctx, fix.env.WANHost, cmd)
	if err != nil {
		t.Logf("%s: ssh burst error: %v", label, err)
	}

	counts := map[string]int{}
	totalOK := 0
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var payload struct {
			InstanceID string `json:"instance_id"`
		}
		if jsonErr := json.Unmarshal([]byte(line), &payload); jsonErr != nil || payload.InstanceID == "" {
			continue
		}
		counts[payload.InstanceID]++
		totalOK++
	}

	for id, n := range counts {
		harness.Detail(t, id, fmt.Sprintf("%d responses", n))
	}
	assert.GreaterOrEqualf(t, len(counts), 2,
		"%s round-robin: expected >=2 unique responders, got %d (counts=%v)", label, len(counts), counts)
	assert.GreaterOrEqualf(t, totalOK, probesPerRun/2,
		"%s success rate: only %d/%d", label, totalOK, probesPerRun)
}

// --- Discovery -----------------------------------------------------------

func discoverNanoInstanceType(t *testing.T, c *harness.AWSClient) string {
	t.Helper()
	out, err := c.EC2.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{})
	require.NoError(t, err, "describe-instance-types")
	for _, it := range out.InstanceTypes {
		name := aws.StringValue(it.InstanceType)
		if strings.Contains(name, "nano") {
			return name
		}
	}
	t.Fatal("no nano instance type available")
	return ""
}

// discoverAMI prefers ubuntu, then non-alpine, then anything — matches
// run-reboot-e2e.sh's three-stage AMI fallback chain.
func discoverAMI(t *testing.T, c *harness.AWSClient) string {
	t.Helper()
	out, err := c.EC2.DescribeImages(&ec2.DescribeImagesInput{})
	require.NoError(t, err, "describe-images")
	var ubuntu, nonAlpine, anyAMI string
	for _, img := range out.Images {
		id := aws.StringValue(img.ImageId)
		name := aws.StringValue(img.Name)
		if anyAMI == "" {
			anyAMI = id
		}
		if !strings.Contains(strings.ToLower(name), "alpine") && nonAlpine == "" {
			nonAlpine = id
		}
		if strings.HasPrefix(name, "ami-ubuntu") {
			ubuntu = id
			break
		}
	}
	for _, candidate := range []string{ubuntu, nonAlpine, anyAMI} {
		if candidate != "" {
			return candidate
		}
	}
	t.Fatal("no AMIs available")
	return ""
}

func describePrivateIP(t *testing.T, c *harness.AWSClient, id string) string {
	t.Helper()
	out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(id)},
	})
	if err != nil {
		t.Logf("describe %s: %v", id, err)
		return ""
	}
	if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return ""
	}
	return aws.StringValue(out.Reservations[0].Instances[0].PrivateIpAddress)
}

func describePublicIP(t *testing.T, c *harness.AWSClient, id string) string {
	t.Helper()
	out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(id)},
	})
	if err != nil {
		t.Logf("describe %s: %v", id, err)
		return ""
	}
	if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return ""
	}
	return aws.StringValue(out.Reservations[0].Instances[0].PublicIpAddress)
}

func describeInstanceState(t *testing.T, c *harness.AWSClient, id string) string {
	t.Helper()
	out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(id)},
	})
	if err != nil {
		return "missing"
	}
	if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return "missing"
	}
	return aws.StringValue(out.Reservations[0].Instances[0].State.Name)
}

func describeLaunchTime(t *testing.T, c *harness.AWSClient, id string) time.Time {
	t.Helper()
	out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(id)},
	})
	if err != nil {
		return time.Time{}
	}
	if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return time.Time{}
	}
	lt := out.Reservations[0].Instances[0].LaunchTime
	if lt == nil {
		return time.Time{}
	}
	return *lt
}

// --- OVN parsing ---------------------------------------------------------

// countOVN extracts logical-switch and port counts from `ovn-nbctl show`
// output by line-prefix matching (matches the bash `grep -c '^switch '` and
// `grep -c '^    port '` checks 1:1).
func countOVN(out string) (switches, ports int) {
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "switch "):
			switches++
		case strings.HasPrefix(line, "    port "):
			ports++
		}
	}
	return
}

func instanceIDFromResponse(out []byte) string {
	var payload struct {
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return ""
	}
	return payload.InstanceID
}

// --- Diagnostics ---------------------------------------------------------

var serviceList = []string{
	"spinifex-nats", "spinifex-predastore", "spinifex-viperblock",
	"spinifex-daemon", "spinifex-awsgw", "spinifex-vpcd",
	"ovn-controller", "ovs-vswitchd", "ovn-northd",
}

func dumpDiagnostics(t *testing.T, fix *fixture, since time.Time) {
	t.Helper()
	var modeFlag string
	if since.IsZero() {
		modeFlag = "-n 200"
		harness.Step(t, "diagnostics: last 200 lines per service")
	} else {
		modeFlag = fmt.Sprintf("--since=@%d", since.Unix())
		harness.Step(t, "diagnostics: since reboot @%d", since.Unix())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if !fix.ssh.IsReachable(ctx, fix.env.WANHost) {
		harness.Step(t, "diagnostics: node unreachable; skipping journal dump")
		return
	}

	for _, svc := range serviceList {
		out, err := fix.ssh.Run(ctx, fix.env.WANHost,
			fmt.Sprintf("sudo journalctl -u %s --no-pager %s 2>/dev/null || true", svc, modeFlag))
		if err != nil {
			harness.Detail(t, svc, fmt.Sprintf("journal err: %v", err))
			continue
		}
		harness.DumpFile(t, fix.artifacts, fmt.Sprintf("journal-%s.log", svc), out)
	}
}

// dumpAppDiagnostics probes each app instance's public IP from the host.
// App VMs have no QEMU hostfwd, so public-IP probes are the only available path.
func dumpAppDiagnostics(t *testing.T, fix *fixture) {
	t.Helper()
	if len(fix.appInstanceIDs) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if !fix.ssh.IsReachable(ctx, fix.env.WANHost) {
		harness.Step(t, "app diagnostics: host unreachable; skipping")
		return
	}

	harness.Step(t, "app diagnostics: probing %d app instance(s)", len(fix.appInstanceIDs))

	dumpHostNetState(ctx, t, fix)
	dumpALBProbe(ctx, t, fix)

	for _, id := range fix.appInstanceIDs {
		diagnoseAppInstance(ctx, t, fix, id)
	}
}

// dumpHostNetState captures OVN/OVS/route state on the host in a single round-trip.
func dumpHostNetState(ctx context.Context, t *testing.T, fix *fixture) {
	t.Helper()
	bundle := `set +e
echo "=== ip route show ==="
ip route show 2>&1
echo "=== ip route show 192.168.0.0/24 ==="
ip route show 192.168.0.0/24 2>&1
echo "=== ip neigh show dev br-wan ==="
ip neigh show dev br-wan 2>&1 || ip neigh show 2>&1
echo "=== ovn-nbctl list nat ==="
sudo ovn-nbctl list nat 2>&1
echo "=== ovn-nbctl show ==="
sudo ovn-nbctl show 2>&1
echo "=== ovs-vsctl show ==="
sudo ovs-vsctl show 2>&1
echo "=== ovs-vsctl list-ports br-int ==="
sudo ovs-vsctl list-ports br-int 2>&1
echo "=== ip -d link show veth-wan-ovs (admin/carrier state) ==="
ip -d link show veth-wan-ovs 2>&1
echo "=== ip -d link show veth-wan-br (admin/carrier state) ==="
ip -d link show veth-wan-br 2>&1
echo "=== ovs-ofctl show br-ext (ofport numbering) ==="
sudo ovs-ofctl show br-ext 2>&1
`
	out, err := fix.ssh.Run(ctx, fix.env.WANHost, bundle)
	harness.DumpFile(t, fix.artifacts, "diag-host-net.txt", out)
	if err != nil {
		harness.Detail(t, "host-net", fmt.Sprintf("dump err: %v", err))
	}
}

// pollTargetsHealthy is the non-fatal twin of harness.WaitForTargetsHealthy:
// returns true on healthy, false on timeout, so the caller can capture diagnostics before aborting.
func pollTargetsHealthy(c *harness.AWSClient, tgArn string, expected int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		out, err := c.ELBv2.DescribeTargetHealth(&elbv2.DescribeTargetHealthInput{
			TargetGroupArn: aws.String(tgArn),
		})
		if err == nil {
			healthy := 0
			for _, th := range out.TargetHealthDescriptions {
				if aws.StringValue(th.TargetHealth.State) == "healthy" {
					healthy++
				}
			}
			if healthy >= expected {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Second)
	}
}

// dumpHeartbeatDiagnostics captures whether the lb-agent's heartbeat to the gateway is
// failing at the network layer or the app/gateway layer. Tcpdumps both the gateway port
// (any iface) and the br-wan north-south path; also pulls lb-agent console and awsgw journal.
func dumpHeartbeatDiagnostics(ctx context.Context, t *testing.T, fix *fixture) {
	t.Helper()
	if fix.albPublicIP == "" {
		harness.Detail(t, "heartbeat", "no ALB public IP captured")
		return
	}
	bundle := fmt.Sprintf(`set +e
echo "=== tcpdump ANY iface, gateway port :%[2]d (real heartbeat path), 14s ==="
sudo timeout 14 tcpdump -ni any -c 80 "tcp port %[2]d" 2>&1
echo "=== tcpdump br-wan ALB floating IP %[1]s + ARP (north-south), 8s ==="
sudo timeout 8 tcpdump -ni br-wan -c 40 "host %[1]s or (arp and host %[1]s)" 2>&1
echo "=== neigh for ALB floating IP %[1]s ==="
ip neigh show %[1]s 2>&1 || ip neigh show 2>&1 | grep -F %[1]s
echo "=== conntrack for gateway port %[2]d ==="
sudo conntrack -L 2>/dev/null | grep -E "dport=%[2]d|sport=%[2]d" | head || echo "(no conntrack entries / tool absent)"
echo "=== lb-agent console: Heartbeat / Config / Agent lines ==="
sudo grep -aE "Heartbeat|Config (hash|applied)|Agent started" /run/spinifex/console-*.log 2>/dev/null | tail -25
echo "=== awsgw journal: did the gateway RECEIVE heartbeats? ==="
sudo journalctl -u spinifex-awsgw --no-pager --since "3 min ago" 2>/dev/null | grep -iE "heartbeat|lb-agent|/lb/|target.?health" | tail -20
`, fix.albPublicIP, fix.env.AWSGWPort)

	out, err := fix.ssh.Run(ctx, fix.env.WANHost, bundle)
	harness.DumpFile(t, fix.artifacts, "diag-heartbeat.txt", out)
	if err != nil {
		harness.Detail(t, "heartbeat", fmt.Sprintf("dump err: %v", err))
		return
	}
	body := string(out)
	// Detect any L4 packet on the gateway port.
	sawGatewayL4 := strings.Contains(body, fmt.Sprintf(".%d:", fix.env.AWSGWPort)) ||
		strings.Contains(body, fmt.Sprintf(".%d >", fix.env.AWSGWPort)) ||
		strings.Contains(body, fmt.Sprintf("> %s.%d", fix.env.WANHost, fix.env.AWSGWPort))
	switch {
	case sawGatewayL4:
		harness.Detail(t, "heartbeat", "gateway-port PACKETS FLOW — network path healthy; failure is app/gateway-layer (TLS/HTTP/gateway state)")
	default:
		harness.Detail(t, "heartbeat", "NO packets on gateway port — network-level drop; check diag-ovn-flows.txt for br-int forwarding")
	}
}

// dumpOVNDataplane captures OVN/OVS dataplane state at failure time: topology, br-int flows,
// Port_Bindings, NAT rows, neigh table, and ALB floating-IP reachability from the host.
func dumpOVNDataplane(ctx context.Context, t *testing.T, fix *fixture) {
	t.Helper()
	bundle := fmt.Sprintf(`set +e
EIP=%[1]s
echo "=== ovs-vsctl show (bridges + ports) ==="
sudo ovs-vsctl show 2>&1 | head -120
echo "=== ovs-ofctl show br-int (port->ofport map) ==="
sudo ovs-ofctl show br-int 2>&1 | grep -aE '\(' | head -50
echo "=== br-int flows: DNAT/conntrack (ct_/nat/ct) ==="
sudo ovs-ofctl dump-flows br-int 2>&1 | grep -aE 'ct_|nat\(|ct\(' | head -120
echo "=== br-int flows: EIP + guest subnet 10.210 (forwarding) ==="
sudo ovs-ofctl dump-flows br-int 2>&1 | grep -aE "nw_dst=$EIP|10\.210\.1\.|192\.168\.0\.21" | head -150
echo "=== br-int flows: terminal output: actions ==="
sudo ovs-ofctl dump-flows br-int 2>&1 | grep -aE 'output:|resubmit' | grep -avE 'table=(0|8|9|10|11),' | head -60
echo "=== ovs-ofctl dump-flows br-ext ==="
sudo ovs-ofctl dump-flows br-ext 2>&1 | head -120
echo "=== ovs-appctl fdb/show br-ext (WAN-side MAC learning) ==="
sudo ovs-appctl fdb/show br-ext 2>&1 | head -40
echo "=== ovn-sbctl Port_Binding (chassis assignment) ==="
sudo ovn-sbctl --no-leader-only list Port_Binding 2>&1 | grep -E "logical_port|chassis|mac|type " | head -80
echo "=== ovn-nbctl dnat_and_snat NAT rows ==="
sudo ovn-nbctl --no-leader-only list NAT 2>&1 | grep -E "external_ip|logical_ip|external_mac|type " | head -60
echo "=== host neigh (all floating IPs) ==="
ip neigh show 2>&1 | grep -E "br-wan|br-int" | head -40
echo "=== REAL ARP to EIP $EIP (flush primed neigh first) ==="
sudo ip neigh flush $EIP 2>&1 || true
ping -c 2 -W 2 $EIP 2>&1
ip neigh show $EIP 2>&1
GWMAC=$(ip neigh show $EIP | awk '{for(i=1;i<=NF;i++) if($i=="lladdr") print $(i+1)}')
echo "discovered cr-gw MAC for EIP: [$GWMAC]"
echo "=== ofproto/trace north-south DNAT (real ingress per br-int patch port, dl_dst=cr-gw) ==="
if [ -n "$GWMAC" ]; then
  for p in $(sudo ovs-vsctl list-ports br-int 2>/dev/null | grep -aE 'patch-br-int-to-ext'); do
    echo "--- in_port=$p ---"
    sudo ovs-appctl ofproto/trace br-int "in_port=$p,tcp,dl_src=02:00:00:00:00:01,dl_dst=$GWMAC,nw_src=192.168.0.11,nw_dst=$EIP,tp_src=40000,tp_dst=80" 2>&1 \
      | grep -aE 'bridge|ct_|nat|dnat|load|output|resubmit|drop|Final flow|Megaflow|Datapath actions' | head -120
  done
else
  echo "(no cr-gw MAC resolved — ARP itself failed)"
fi
echo "=== south->north RETURN trace (guest reply -> un-DNAT -> WAN) ==="
PRIV=$(sudo ovn-nbctl --no-leader-only --bare --columns=logical_ip find NAT external_ip="\"$EIP\"" 2>/dev/null | head -1)
CLIENT=$({ sudo ovs-appctl dpctl/dump-conntrack 2>/dev/null || sudo conntrack -L 2>/dev/null; } | grep -aE "dst=$EIP" | grep -aoE 'src=[0-9.]+' | head -1 | cut -d= -f2)
[ -z "$CLIENT" ] && CLIENT=192.168.1.1
GLSP=""; GMAC=""
for lp in $(sudo ovn-sbctl --no-leader-only --bare --columns=logical_port find Port_Binding type='""' 2>/dev/null); do
  m=$(sudo ovn-sbctl --no-leader-only --bare --columns=mac find Port_Binding logical_port="$lp" 2>/dev/null)
  case "$m" in *"$PRIV"*) GLSP="$lp"; GMAC=$(echo "$m" | awk '{print $1}'); break;; esac
done
TAP=$(sudo ovs-vsctl --bare --columns=name find Interface external_ids:iface-id="$GLSP" 2>/dev/null | head -1)
SUBNET_GW=$(echo "$PRIV" | awk -F. '{print $1"."$2"."$3".1"}')
LRPMAC=$(sudo ovn-nbctl --no-leader-only --bare --columns=mac find Logical_Router_Port networks~="$SUBNET_GW/" 2>/dev/null | head -1)
echo "PRIV=[$PRIV] CLIENT=[$CLIENT] GLSP=[$GLSP] GMAC=[$GMAC] TAP=[$TAP] LRPMAC=[$LRPMAC]"
if [ -n "$TAP" ] && [ -n "$GMAC" ] && [ -n "$LRPMAC" ]; then
  sudo ovs-appctl ofproto/trace br-int "in_port=$TAP,tcp,dl_src=$GMAC,dl_dst=$LRPMAC,nw_src=$PRIV,nw_dst=$CLIENT,tp_src=80,tp_dst=40000,tcp_flags=ack" 2>&1 \
    | grep -aE 'bridge|ct_|nat|dnat|snat|load|output|resubmit|drop|Final flow|Megaflow|Datapath actions' | head -150
else
  echo "(return-trace discovery incomplete — skipping)"
fi
echo "=== conntrack DNAT entries (EIP/guest) ==="
{ sudo ovs-appctl dpctl/dump-conntrack 2>/dev/null || sudo conntrack -L 2>/dev/null; } | grep -aE "$EIP|10\.210\.1\." | head -30 || echo "(none)"
echo "=== tap capture: does the SYN reach the guest? (tcpdump -i any while curling EIP) ==="
sudo timeout 6 tcpdump -i any -nn -c 25 "host $EIP or net 10.210.0.0/16" 2>&1 &
TCPID=$!
sleep 1
curl -s --connect-timeout 2 --max-time 4 "http://$EIP/" >/dev/null 2>&1 &
wait $TCPID 2>/dev/null
echo "=== (end tap capture) ==="
`, fix.albPublicIP)

	out, err := fix.ssh.Run(ctx, fix.env.WANHost, bundle)
	harness.DumpFile(t, fix.artifacts, "diag-ovn-flows.txt", out)
	if err != nil {
		harness.Detail(t, "ovn", fmt.Sprintf("dump err: %v", err))
		return
	}
	body := string(out)
	reachable := strings.Contains(body, " 0% packet loss")
	hasFlows := strings.Contains(body, "cookie=") && strings.Contains(body, "table=")
	switch {
	case reachable:
		harness.Detail(t, "ovn", "ALB floating IP reachable from host — forwarding OK, failure is elsewhere")
	case hasFlows:
		harness.Detail(t, "ovn", "br-int has flows but floating IP unreachable — flow/forwarding mismatch (inspect dump-flows + ofproto/trace)")
	default:
		harness.Detail(t, "ovn", "br-int flows MISSING and floating IP unreachable — dataplane flow-restore gap post-reboot")
	}
}

// ovnChassisStateBundle captures OVN chassis identity + SB records for pre/post diff.
// _uuid churn across reboot orphans every Port_Binding claim.
const ovnChassisStateBundle = `set +e
echo "=== OVS system-id (chassis identity; must be stable across reboot) ==="
sudo cat /etc/openvswitch/system-id.conf 2>&1
sudo ovs-vsctl get Open_vSwitch . external_ids:system-id 2>&1
echo "=== ovn-sbctl list Chassis (name/_uuid/hostname/encaps) ==="
sudo ovn-sbctl --no-leader-only list Chassis 2>&1 | grep -E "_uuid|name|hostname|ip " | head -40
echo "=== ovn-sbctl list Chassis_Private (claimed-by + nb_cfg) ==="
sudo ovn-sbctl --no-leader-only list Chassis_Private 2>&1 | grep -E "_uuid|name|chassis|nb_cfg" | head -40
echo "=== ovn get-connection (did set-connection persist?) ==="
sudo ovn-nbctl get-connection 2>&1
sudo ovn-sbctl get-connection 2>&1
`

// dumpOVNClaimState captures evidence for why ovn-controller is not claiming Port_Bindings.
// Pulls /var/log/ovn directly (not journald), SB connection state, and boot ordering
// to distinguish chassis-identity churn from an SB/NB startup-ordering race.
func dumpOVNClaimState(ctx context.Context, t *testing.T, fix *fixture) {
	t.Helper()
	bundle := ovnChassisStateBundle + `echo "=== ovn-controller SB connection-status ==="
sudo env OVS_RUNDIR=/var/run/ovn ovs-appctl -t ovn-controller connection-status 2>&1
echo "=== ovn-controller debug/status ==="
sudo env OVS_RUNDIR=/var/run/ovn ovs-appctl -t ovn-controller debug/status 2>&1 | head -20
echo "=== gw/eni Port_Binding claim detail (chassis/up/requested_chassis) ==="
sudo ovn-sbctl --no-leader-only --columns=logical_port,type,chassis,up,requested_chassis find Port_Binding 2>&1 | grep -A4 -E "gw-vpc|port-eni" | head -60
echo "=== ovn-central vs ovn-controller boot ordering ==="
systemctl show ovn-central ovn-controller openvswitch-switch -p Id -p ActiveEnterTimestamp -p ExecMainStartTimestamp 2>&1
echo "=== tail /var/log/ovn/ovn-controller.log ==="
sudo tail -n 60 /var/log/ovn/ovn-controller.log 2>&1
echo "=== tail /var/log/ovn/ovn-northd.log ==="
sudo tail -n 40 /var/log/ovn/ovn-northd.log 2>&1
`
	out, err := fix.ssh.Run(ctx, fix.env.WANHost, bundle)
	harness.DumpFile(t, fix.artifacts, "diag-ovn-claim.txt", out)
	if err != nil {
		harness.Detail(t, "ovn-claim", fmt.Sprintf("dump err: %v", err))
		return
	}
	body := string(out)
	switch {
	case strings.Contains(body, "not connected"):
		harness.Detail(t, "ovn-claim", "ovn-controller NOT connected to SB — claim failure is an SB-connection/ordering issue")
	case strings.Contains(body, "chassis             : []"), strings.Contains(body, "chassis : []"):
		harness.Detail(t, "ovn-claim", "gw/eni Port_Binding unclaimed (chassis empty) — diff ovn-pre-chassis.txt vs this for identity churn")
	default:
		harness.Detail(t, "ovn-claim", "see diag-ovn-claim.txt — compare Chassis _uuid against ovn-pre-chassis.txt")
	}
}

// dumpALBProbe probes the ALB public IP from host — pre-reboot this path
// worked (the burst hit it). If post-reboot ALB is also unreachable, the
// gateway/ext-bridge is broken; if ALB reaches but apps don't, only the
// app-VM side is broken. Complements per-app probes.
func dumpALBProbe(ctx context.Context, t *testing.T, fix *fixture) {
	t.Helper()
	if fix.albPublicIP == "" {
		harness.Detail(t, "alb", "no public IP captured")
		return
	}
	probe := fmt.Sprintf(`set +e
echo "=== ping ALB %[1]s ==="
ping -c 2 -W 2 %[1]s 2>&1
echo "=== tcp-connect ALB %[1]s:80 ==="
timeout 5 bash -c 'exec 3<>/dev/tcp/%[1]s/80 && echo "tcp OPEN" || echo "tcp CLOSED/UNREACH"'
echo "=== curl ALB http://%[1]s/ ==="
curl -sS -m 5 -w 'HTTP=%%{http_code} time=%%{time_total}s\n' http://%[1]s/ 2>&1
`, fix.albPublicIP)
	out, err := fix.ssh.Run(ctx, fix.env.WANHost, probe)
	harness.DumpFile(t, fix.artifacts, "diag-alb-probe.txt", out)
	if err != nil {
		harness.Detail(t, "alb", fmt.Sprintf("probe err: %v", err))
		return
	}
	body := string(out)
	switch {
	case strings.Contains(body, `"instance_id"`):
		harness.Detail(t, "alb", "ALB serving (responder reachable through HAProxy)")
	case strings.Contains(body, "tcp OPEN"):
		harness.Detail(t, "alb", "ALB tcp open, no body (HAProxy up but backends silent)")
	case strings.Contains(body, "tcp CLOSED"):
		harness.Detail(t, "alb", "ALB tcp closed/unreach (gateway or sys.micro broken)")
	default:
		harness.Detail(t, "alb", "probe inconclusive — see artifact")
	}
}

// diagnoseAppInstance probes one instance from the host VM: qemu running,
// ping, TCP-connect on :80, HTTP GET. Single shell session per instance.
func diagnoseAppInstance(ctx context.Context, t *testing.T, fix *fixture, id string) {
	t.Helper()

	psOut, err := fix.ssh.Run(ctx, fix.env.WANHost,
		fmt.Sprintf("ps -eo args= | grep -F %q | grep -v grep || true", id))
	harness.DumpFile(t, fix.artifacts, fmt.Sprintf("diag-%s-qemu-ps.txt", id), psOut)
	switch {
	case err != nil:
		harness.Detail(t, id, fmt.Sprintf("qemu ps err: %v", err))
	case len(strings.TrimSpace(string(psOut))) == 0:
		harness.Detail(t, id, "qemu NOT running on host")
	default:
		harness.Detail(t, id, "qemu running")
	}

	// Serial console (ttyS0) — cloud-init, kernel oops, systemd boot events.
	consoleOut, _ := fix.ssh.Run(ctx, fix.env.WANHost,
		fmt.Sprintf("sudo tail -c 262144 /run/spinifex/console-%s.log 2>/dev/null || true", id))
	harness.DumpFile(t, fix.artifacts, fmt.Sprintf("diag-%s-console.log", id), consoleOut)

	priv := fix.preIPs[id]
	pub := describePublicIP(t, fix.aws, id)
	harness.Detail(t, id, fmt.Sprintf("priv=%s pub=%s", priv, pub))
	if pub == "" {
		harness.Detail(t, id, "no public IP — cannot probe from host")
		return
	}

	probe := fmt.Sprintf(`set +e
echo "=== ping %[1]s ==="
ping -c 2 -W 2 %[1]s 2>&1
echo "=== tcp-connect %[1]s:80 ==="
timeout 5 bash -c 'exec 3<>/dev/tcp/%[1]s/80 && echo "tcp OPEN" || echo "tcp CLOSED/UNREACH"'
echo "=== curl http://%[1]s/ ==="
curl -sS -m 5 -o /tmp/diag-body-%[2]s -w 'HTTP=%%{http_code} time=%%{time_total}s size=%%{size_download}\n' http://%[1]s/
echo "=== body ==="
cat /tmp/diag-body-%[2]s 2>/dev/null; echo
rm -f /tmp/diag-body-%[2]s
`, pub, id)

	out, err := fix.ssh.Run(ctx, fix.env.WANHost, probe)
	harness.DumpFile(t, fix.artifacts, fmt.Sprintf("diag-%s-net-probe.txt", id), out)
	if err != nil {
		harness.Detail(t, id, fmt.Sprintf("probe err: %v", err))
		return
	}

	body := string(out)
	switch {
	case strings.Contains(body, `"instance_id"`):
		harness.Detail(t, id, "responder OK from host")
	case strings.Contains(body, "tcp OPEN"):
		harness.Detail(t, id, "tcp open but responder silent")
	case strings.Contains(body, "tcp CLOSED"):
		harness.Detail(t, id, "tcp closed/unreach (responder down OR OVN path broken)")
	default:
		harness.Detail(t, id, "probe inconclusive — see artifact")
	}

	// SSH-in via IGW; dumps responder unit + cloud-init state inside the guest.
	sshIntoApp(ctx, t, fix, id, pub)
}

// sshIntoApp uploads the key to the host then SSHes into the app guest to dump
// responder, cloud-init, and network state. Best-effort: no-ops if key unavailable or SSH fails.
func sshIntoApp(ctx context.Context, t *testing.T, fix *fixture, id, pub string) {
	t.Helper()
	if fix.privateKeyPath == "" {
		return
	}
	keyData, err := os.ReadFile(fix.privateKeyPath)
	if err != nil {
		harness.Detail(t, id, fmt.Sprintf("ssh-in: read key err: %v", err))
		return
	}
	enc := base64.StdEncoding.EncodeToString(keyData)
	remoteKey := fmt.Sprintf("/tmp/reboot-e2e-%s.pem", id)

	diag := `set +e
echo "=== uname / uptime ==="
uname -a; uptime
echo "=== systemctl is-active reboot-e2e-httpd.service ==="
systemctl is-active reboot-e2e-httpd.service
systemctl is-enabled reboot-e2e-httpd.service
echo "=== systemctl status (head 40) ==="
systemctl status --no-pager reboot-e2e-httpd.service 2>&1 | head -40
echo "=== unit file ==="
ls -l /etc/systemd/system/reboot-e2e-httpd.service 2>&1
echo "=== unit file content ==="
sudo cat /etc/systemd/system/reboot-e2e-httpd.service 2>&1
echo "=== enable symlink ==="
ls -l /etc/systemd/system/multi-user.target.wants/reboot-e2e-httpd.service 2>&1
echo "=== network-online.target ==="
systemctl is-active network-online.target
systemctl status --no-pager network-online.target 2>&1 | head -20
echo "=== networkctl ==="
networkctl status 2>&1 | head -40
echo "=== ip addr ==="
ip -br addr
echo "=== ip route ==="
ip route
echo "=== port :80 listeners ==="
sudo ss -tlnp 'sport = :80' 2>/dev/null
echo "=== curl localhost ==="
curl -s -m 3 -w '\nHTTP=%{http_code}\n' http://127.0.0.1/ || echo "curl localhost FAILED"
echo "=== /var/lib/httpd ==="
ls -l /var/lib/httpd 2>&1
cat /var/lib/httpd/index.html 2>&1
echo "=== responder journal ==="
sudo journalctl -u reboot-e2e-httpd.service --no-pager -n 60 2>&1
echo "=== boot list ==="
sudo journalctl --list-boots 2>&1 | tail -5
echo "=== last-boot journal grep responder ==="
sudo journalctl -b -1 --no-pager 2>&1 | grep -i "reboot-e2e\|network-online\|cloud-init" | tail -40
echo "=== this-boot journal grep responder ==="
sudo journalctl -b --no-pager 2>&1 | grep -i "reboot-e2e\|network-online\|cloud-init" | tail -40
echo "=== cloud-init result.json ==="
cat /run/cloud-init/result.json 2>&1
echo "=== cloud-init.log tail ==="
sudo tail -80 /var/log/cloud-init.log 2>&1
echo "=== cloud-init-output.log tail ==="
sudo tail -40 /var/log/cloud-init-output.log 2>&1
echo "=== instance-id sem ==="
ls -l /var/lib/cloud/instance 2>&1
ls -l /var/lib/cloud/instances/ 2>&1
echo "=== /var/lib/cloud/instances/*/sem ==="
ls -l /var/lib/cloud/instances/*/sem/ 2>&1
`
	// Single outer SSH: write the key then hop into the guest. One round trip.
	outerCmd := fmt.Sprintf(`set +e
umask 077 && printf '%%s' '%s' | base64 -d > %s && chmod 600 %s
ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=8 -o BatchMode=yes -o ServerAliveInterval=5 \
    ec2-user@%s bash -s <<'EOFDIAG'
%s
EOFDIAG
rm -f %s
`, enc, remoteKey, remoteKey, remoteKey, pub, diag, remoteKey)

	out, err := fix.ssh.Run(ctx, fix.env.WANHost, outerCmd)
	harness.DumpFile(t, fix.artifacts, fmt.Sprintf("diag-%s-guest-ssh.txt", id), out)
	if err != nil {
		harness.Detail(t, id, fmt.Sprintf("ssh-in err: %v", err))
		return
	}
	harness.Detail(t, id, "ssh-in OK (see diag-*-guest-ssh.txt)")
}

// --- Cleanup -------------------------------------------------------------

// cleanup tears down all test resources; SG is deleted last as it can't be removed
// while instance or ALB ENIs still reference it.
func cleanup(t *testing.T, fix *fixture) {
	t.Helper()
	harness.Phase(t, "Cleanup")

	if !fix.ssh.IsReachable(context.Background(), fix.env.WANHost) {
		harness.Step(t, "cleanup skipped: node unreachable")
		return
	}

	if fix.listenerArn != "" {
		_, _ = fix.aws.ELBv2.DeleteListener(&elbv2.DeleteListenerInput{ListenerArn: aws.String(fix.listenerArn)})
	}
	if fix.albArn != "" {
		_, _ = fix.aws.ELBv2.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{LoadBalancerArn: aws.String(fix.albArn)})
	}
	if fix.tgArn != "" {
		_, _ = fix.aws.ELBv2.DeleteTargetGroup(&elbv2.DeleteTargetGroupInput{TargetGroupArn: aws.String(fix.tgArn)})
	}

	if len(fix.appInstanceIDs) > 0 {
		ids := make([]*string, len(fix.appInstanceIDs))
		for i, id := range fix.appInstanceIDs {
			ids[i] = aws.String(id)
		}
		_, _ = fix.aws.EC2.TerminateInstances(&ec2.TerminateInstancesInput{InstanceIds: ids})
		harness.WaitForInstanceTerminated(t, fix.aws, fix.appInstanceIDs, 60*time.Second)
	}

	_, _ = fix.aws.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(keyName)})

	// SG must come after instances + ALB ENIs are gone.
	if fix.sgID != "" {
		if _, err := fix.aws.EC2.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(fix.sgID),
		}); err != nil {
			var aerr awserr.Error
			if !errors.As(err, &aerr) || aerr.Code() != "InvalidGroup.NotFound" {
				t.Logf("delete SG %s: %v", fix.sgID, err)
			}
		}
	}

	if fix.igwID != "" && fix.vpcID != "" {
		_, _ = fix.aws.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(fix.igwID),
			VpcId:             aws.String(fix.vpcID),
		})
		_, _ = fix.aws.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
			InternetGatewayId: aws.String(fix.igwID),
		})
	}
	if fix.subnetID != "" {
		_, _ = fix.aws.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(fix.subnetID)})
	}
	if fix.vpcID != "" {
		_, _ = fix.aws.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(fix.vpcID)})
	}
}

// --- Misc ----------------------------------------------------------------

func durationEnv(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	// Accept a bare integer (treated as seconds) or a time.ParseDuration string.
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if d, err := time.ParseDuration(v + "s"); err == nil {
		return d
	}
	return def
}
