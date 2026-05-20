//go:build e2e

// Package reboot ports run-reboot-e2e.sh — the single-node host-reboot
// resilience driver (cell #18).
//
// Unlike the cert/lb/single suites which ship their compiled .test binary to
// the spinifex VM and run in-VM, this suite MUST run on the GitHub Actions
// runner (or any host outside the cluster). `sudo systemctl reboot` kills any
// in-VM process mid-step, so the driver has to live somewhere the reboot
// can't touch.
//
// All AWS API traffic crosses the WAN to https://WAN_IP:9999 via the SDK;
// SSH-level ops (reboot, ovn-nbctl, journalctl, file mutations) hop via
// harness.PeerSSH.
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

// appUserData provisions a systemd-managed HTTP responder on the guest VM.
// Installed as a unit rather than nohup-from-cloud-init so the responder
// survives the guest's own restart after the host reboot — cloud-init's
// per-instance semaphore only fires once.
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

// TestRebootResilience is the Go port of run-reboot-e2e.sh.
//
// Phases run sequentially in one top-level test rather than parallel subtests
// — the reboot itself is a global event and later phases assume earlier
// phases succeeded. Each phase emits a Phase banner so failures localise
// cleanly in JUnit XML.
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
			// 5min, not bash's 180s. Pre-reboot the test creates the LB
			// AFTER the apps are already running, so by the time HAProxy
			// starts probing the app responders are already serving — ~75s
			// to healthy. Post-reboot the daemon's recovery scan relaunches
			// app VMs and the ALB sys.micro VM simultaneously, so HC starts
			// probing within seconds of app boot beginning. Both targets
			// honestly report unhealthy until the app's responder unit
			// finishes starting (cloud-init's per-instance semaphore means
			// no shortcut path on relaunch). The bash driver masked this by
			// reading stale "healthy" from the daemon's pre-reboot TG cache
			// (now fixed via ResetTargetHealthOnStartup — mulga-siv-119);
			// the honest wait surfaces the real recovery window.
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

// requireRunnerMode bails early if the env doesn't look runner-resident.
// WAN_IP empty would still work but signals a config mistake — better to fail
// fast than walk through 30s of AWS-endpoint timeouts before the operator
// notices.
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

// resolveTrust attempts to SCP the spinifex CA off the VM to a tmp file so
// the AWS SDK uses real TLS verification. On any failure (no SSH key, scp
// non-zero exit, file empty) it logs the reason and flips
// SPINIFEX_AWS_INSECURE=1 so the SDK skips verification. CA trust isn't what
// the reboot suite is testing — the cert suite already validates trust — so
// the fallback is intentional rather than a quiet degradation.
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

	// Key pair — capture KeyMaterial so post-reboot diagnostics can SSH into
	// guest VMs via the host's QEMU hostfwd port when health probes fail.
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

	// Structured IpPermissions form — vpcd ignores the top-level shortcut
	// (mulga-siv-79) so port 80 ingress must use IpPermissions to land in OVN.
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
}

func phase6Reboot(t *testing.T, fix *fixture) {
	t.Helper()
	fix.mu.Lock()
	fix.rebootStart = time.Now()
	fix.mu.Unlock()
	harness.Step(t, "issuing reboot via ssh (connection will drop)")

	// Best-effort: the ssh process is expected to die mid-command when the
	// host starts shutting down. Don't fail on it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = fix.ssh.Run(ctx, fix.env.WANHost, "sudo systemctl reboot")

	// Let the host start shutting down before we begin polling so we don't
	// accept the pre-reboot SSH window as "back".
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

	// 8.5 targets healthy
	harness.WaitForTargetsHealthy(t, fix.aws, fix.tgArn, 2, "ALB post-reboot", fix.timeouts.lbRecover)

	// 8.6 ALB actually serves traffic from BOTH backends.
	// WaitForTargetsHealthy reads cached state from spinifex's TG store —
	// post-reboot the cache may still show pre-reboot "healthy" until the
	// lb-agent inside the ALB system VM sends a fresh report. A naive
	// "ANY response" probe lets the burst fire when only one backend is
	// up (the second is still bringing HAProxy/responder online), and the
	// burst then lands 20/0 instead of 10/10. Probe until both backends'
	// instance_ids each appear at least once — confirms the recovery path
	// has actually reached both before we assert round-robin. Refs the
	// daemon-side TG cache invalidation bug tracked separately.
	harness.Step(t, "waiting for ALB to serve HTTP from both backends")
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer probeCancel()
	seen := map[string]bool{}
	harness.Eventually(t, func() bool {
		out, err := fix.ssh.Run(probeCtx, fix.env.WANHost,
			fmt.Sprintf("curl -s --connect-timeout 2 --max-time 3 'http://%s:%d/' 2>/dev/null", fix.albPublicIP, httpPort))
		if err != nil {
			return false
		}
		id := instanceIDFromResponse(out)
		if id != "" {
			seen[id] = true
		}
		return len(seen) >= 2
	}, 180*time.Second, 1*time.Second,
		fmt.Sprintf("ALB did not serve from both backends within 180s post-reboot (saw=%v)", seen))

	runHTTPBurst(t, fix, "ALB post-reboot")

	// 8.6.5 ALWAYS-RUN guest probe — captures in-guest responder state even
	// when the test passes, so we can confirm whether the responder is genuinely
	// running on the relaunched VMs vs. some upstream cache satisfying HC.
	// Refs investigation into "tcp/22 SG ingress fixes post-reboot HTTP".
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

// dumpAppDiagnostics probes each app instance's IGW-NAT'd public IP from the
// host VM. App VMs use pure OVN tap interfaces (no QEMU hostfwd), so SSH from
// host is impossible — public-IP probes are the only no-cost path. Localises
// post-reboot failures to one of: VM not running, OVN DNAT missing, responder
// down, or responder up but reachable only from sys.micro (path asymmetry).
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

// dumpHostNetState captures post-reboot OVN/OVS/route state on the spinifex
// host so failures show whether the gateway router, ext bridge, and host
// routing table all came back as expected. Single round-trip.
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
`
	out, err := fix.ssh.Run(ctx, fix.env.WANHost, bundle)
	harness.DumpFile(t, fix.artifacts, "diag-host-net.txt", out)
	if err != nil {
		harness.Detail(t, "host-net", fmt.Sprintf("dump err: %v", err))
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

	// Serial console — captures cloud-init, kernel oops, systemd boot, anything
	// the guest emitted to ttyS0. Tail to cap size; the full log lives on the
	// host until cleanup.
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

	// Try SSH-in via the IGW path. tcp/22 SG ingress is wired in phase1.
	// On success this dumps the responder unit + cloud-init state, which is
	// the only path to root-cause a "tcp open but responder silent" failure.
	sshIntoApp(ctx, t, fix, id, pub)
}

// sshIntoApp uploads the key pair private key to the host, then SSHes from
// the host to the app guest at its IGW public IP and dumps responder /
// cloud-init / network state. Best-effort: silently no-ops if key wasn't
// captured or SSH fails.
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
	// Outer ssh ships one cmd; inside, write the key, then ssh again to guest
	// with a heredoc. Single round trip from runner.
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

// cleanup runs t.Cleanup logic — LIFO ordering of the bash cleanup() trap.
// SG deletion runs LAST because it can't be deleted while instance/ALB ENIs
// still reference it.
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
	// Accept either a bare integer (seconds, per the bash defaults) or any
	// time.ParseDuration string (e.g. "5m"). Bash sets REBOOT_WAIT_SECS=300.
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if d, err := time.ParseDuration(v + "s"); err == nil {
		return d
	}
	return def
}
