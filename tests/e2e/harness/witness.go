//go:build e2e

package harness

import (
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"golang.org/x/crypto/ssh"
)

// cloudInitWitness is the user-data payload that turns a fresh cloud-init VM
// into a monotonic counter. See testdata/cloud-init-witness.yaml for the
// source; embedded so the harness is self-contained.
//
//go:embed testdata/cloud-init-witness.yaml
var cloudInitWitness []byte

// gatewayPort is the AWS gateway port on each cluster node. Matches
// tests/e2e/run-multinode-e2e.sh's AWSGW_PORT.
const gatewayPort = 9999

// Witness bundles the AWS SDK client, guest/host SSH credentials, and AMI
// selection needed to launch and interrogate counter VMs. One Witness is
// shared across the scenarios in a suite.
type Witness struct {
	ec2          *ec2.EC2
	cluster      *Cluster
	ssh          SSH
	hostSigner   ssh.Signer
	guestSigner  ssh.Signer
	guestUser    string
	ami          string
	instanceType string
	keyName      string
}

// NewWitness constructs a Witness from environment variables. Required:
// AWS_REGION, DDIL_GUEST_SSH_KEY. Optional: DDIL_WITNESS_AMI,
// DDIL_WITNESS_INSTANCE_TYPE (defaults to smallest registered type),
// DDIL_WITNESS_KEY_NAME (default spinifex-key), DDIL_GUEST_SSH_USER (default ubuntu).
func NewWitness(cluster *Cluster, transport SSH) (*Witness, error) {
	region := os.Getenv("AWS_REGION")
	if region == "" {
		return nil, errors.New("e2e harness: NewWitness requires AWS_REGION")
	}
	if len(cluster.Nodes) == 0 {
		return nil, errors.New("e2e harness: NewWitness requires a non-empty cluster")
	}

	endpoint := "https://" + net.JoinHostPort(cluster.Nodes[0].Addr, strconv.Itoa(gatewayPort))
	sess, err := session.NewSession(&aws.Config{
		Region:   aws.String(region),
		Endpoint: aws.String(endpoint),
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed test certs
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("e2e harness: aws session: %w", err)
	}

	hostSigner, err := loadSigner(cluster.SSHKeyPath)
	if err != nil {
		return nil, fmt.Errorf("e2e harness: host ssh key: %w", err)
	}

	guestKeyPath := os.Getenv("DDIL_GUEST_SSH_KEY")
	if guestKeyPath == "" {
		return nil, errors.New("e2e harness: NewWitness requires DDIL_GUEST_SSH_KEY (path to the private key paired with DDIL_WITNESS_KEY_NAME's registered material)")
	}
	guestSigner, err := loadSigner(guestKeyPath)
	if err != nil {
		return nil, fmt.Errorf("e2e harness: guest ssh key: %w", err)
	}

	return &Witness{
		ec2:          ec2.New(sess),
		cluster:      cluster,
		ssh:          transport,
		hostSigner:   hostSigner,
		guestSigner:  guestSigner,
		guestUser:    envDefault("DDIL_GUEST_SSH_USER", "ubuntu"),
		ami:          os.Getenv("DDIL_WITNESS_AMI"),
		instanceType: os.Getenv("DDIL_WITNESS_INSTANCE_TYPE"),
		keyName:      envDefault("DDIL_WITNESS_KEY_NAME", "spinifex-key"),
	}, nil
}

// WitnessVM is the live descriptor for a single counter VM. Created by
// LaunchWitnessVM; subsequent ReadCounter / AssertProgressed calls use the
// back-reference to the owning Witness to avoid passing the SDK/SSH deps
// through every call site.
type WitnessVM struct {
	InstanceID      string
	HostNode        Node   // cluster node the VM's QEMU process lives on
	PublicIP        string // EIP allocated by MapPublicIpOnLaunch; SSH target reachable from the runner
	LaunchedAt      time.Time
	BaselineCounter int

	w *Witness
}

// LaunchWitnessVM launches a counter VM and waits for its QEMU process to
// appear on host. The returned WitnessVM carries the host/port it was placed
// on (which may differ from host if the scheduler rejected the hint — a
// best-effort retry up to maxPlacementAttempts is performed before giving up).
//
// Depends on ami-ubuntu-* being registered in the cluster (the tofu-cluster
// bootstrap handles this) and on the AWS credential chain resolving, so
// scenarios gate on t.Skip when neither is available.
func LaunchWitnessVM(ctx context.Context, w *Witness, host Node) (*WitnessVM, error) {
	const maxPlacementAttempts = 3

	// Workaround: daemon silently drops the shortcut SG ingress form, so
	// the default SG starts with no :22 ingress and the awaitBaseline SSH
	// tunnel to the witness EIP times out. Authorise :22 from anywhere
	// via the structured form — once the underlying bug lands this can
	// drop along with the matching workaround in lb_test.
	if err := w.ensureDefaultSGSSHIngress(ctx); err != nil {
		return nil, fmt.Errorf("e2e harness: open witness SSH ingress: %w", err)
	}

	ami, err := w.resolveAMI(ctx)
	if err != nil {
		return nil, err
	}
	instanceType, err := w.resolveInstanceType(ctx)
	if err != nil {
		return nil, err
	}

	userData := base64.StdEncoding.EncodeToString(cloudInitWitness)

	var lastErr error
	for attempt := 1; attempt <= maxPlacementAttempts; attempt++ {
		out, err := w.ec2.RunInstancesWithContext(ctx, &ec2.RunInstancesInput{
			ImageId:      aws.String(ami),
			InstanceType: aws.String(instanceType),
			MinCount:     aws.Int64(1),
			MaxCount:     aws.Int64(1),
			KeyName:      aws.String(w.keyName),
			UserData:     aws.String(userData),
		})
		if err != nil {
			return nil, fmt.Errorf("e2e harness: RunInstances: %w", err)
		}
		if len(out.Instances) == 0 || out.Instances[0].InstanceId == nil {
			return nil, errors.New("e2e harness: RunInstances returned no instance")
		}
		id := aws.StringValue(out.Instances[0].InstanceId)

		if err := w.waitForRunning(ctx, id, 2*time.Minute); err != nil {
			_ = w.terminate(ctx, id)
			return nil, err
		}

		placed, err := w.findHost(ctx, id)
		if err != nil {
			_ = w.terminate(ctx, id)
			return nil, err
		}

		if placed.Index == host.Index {
			publicIP, err := w.resolvePublicIP(ctx, id)
			if err != nil {
				_ = w.terminate(ctx, id)
				return nil, err
			}
			baseline, err := awaitBaseline(ctx, w, placed, publicIP, 2*time.Minute)
			if err != nil {
				_ = w.terminate(ctx, id)
				return nil, fmt.Errorf("e2e harness: witness baseline read: %w", err)
			}
			return &WitnessVM{
				InstanceID:      id,
				HostNode:        placed,
				PublicIP:        publicIP,
				LaunchedAt:      time.Now(),
				BaselineCounter: baseline,
				w:               w,
			}, nil
		}

		lastErr = fmt.Errorf("witness landed on %s, wanted %s", placed.Name, host.Name)
		_ = w.terminate(ctx, id)
	}
	return nil, fmt.Errorf("e2e harness: LaunchWitnessVM: %w after %d attempts", lastErr, maxPlacementAttempts)
}

// ReadCounter SSHes the guest at its public IP (tunnelled through the
// hosting cluster node) and returns the current /var/lib/counter value.
func (v *WitnessVM) ReadCounter(ctx context.Context) (int, error) {
	return readCounter(ctx, v.w, v.HostNode, v.PublicIP)
}

// Terminate asks EC2 to shut the witness down. Scenarios call this from
// t.Cleanup so a failed assertion does not leak a counter VM onto the
// cluster between attempts.
func (v *WitnessVM) Terminate(ctx context.Context) error {
	return v.w.terminate(ctx, v.InstanceID)
}

// AssertProgressed fails t if the counter has not advanced beyond the value
// captured at launch. Uses t.Helper so the caller's line number lands in the
// failure message.
func AssertProgressed(ctx context.Context, t *testing.T, v *WitnessVM) {
	t.Helper()
	current, err := v.ReadCounter(ctx)
	if err != nil {
		t.Fatalf("e2e harness: read witness counter on %s (%s): %v", v.HostNode.Name, v.InstanceID, err)
	}
	if current <= v.BaselineCounter {
		t.Fatalf("e2e harness: witness %s on %s did not progress: baseline=%d current=%d",
			v.InstanceID, v.HostNode.Name, v.BaselineCounter, current)
	}
	t.Logf("witness %s on %s progressed %d → %d", v.InstanceID, v.HostNode.Name, v.BaselineCounter, current)
}

// --- internals ------------------------------------------------------------

// ensureDefaultSGSSHIngress authorises tcp/22 + ICMP from 0.0.0.0/0 on the
// default security group via the structured IpPermissions form. The daemon
// currently drops the top-level shortcut form silently, so without this the
// witness EIP has no inbound :22 and awaitBaseline times out. ICMP is
// required for downstream gateway-ping probes.
// Idempotent: duplicate-rule errors from a prior run are tolerated.
func (w *Witness) ensureDefaultSGSSHIngress(ctx context.Context) error {
	sgs, err := w.ec2.DescribeSecurityGroupsWithContext(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("group-name"),
			Values: []*string{aws.String("default")},
		}},
	})
	if err != nil {
		return fmt.Errorf("DescribeSecurityGroups: %w", err)
	}
	if len(sgs.SecurityGroups) == 0 || sgs.SecurityGroups[0].GroupId == nil {
		return errors.New("no default security group found")
	}
	groupID := aws.StringValue(sgs.SecurityGroups[0].GroupId)

	rules := []*ec2.IpPermission{
		{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(22),
			ToPort:     aws.Int64(22),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
		},
		{
			IpProtocol: aws.String("icmp"),
			FromPort:   aws.Int64(-1),
			ToPort:     aws.Int64(-1),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
		},
	}
	for _, rule := range rules {
		_, err = w.ec2.AuthorizeSecurityGroupIngressWithContext(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId:       aws.String(groupID),
			IpPermissions: []*ec2.IpPermission{rule},
		})
		if err != nil && !strings.Contains(err.Error(), "InvalidPermission.Duplicate") {
			return fmt.Errorf("AuthorizeSecurityGroupIngress %s on %s: %w",
				aws.StringValue(rule.IpProtocol), groupID, err)
		}
	}
	return nil
}

func (w *Witness) resolveAMI(ctx context.Context) (string, error) {
	if w.ami != "" {
		return w.ami, nil
	}
	out, err := w.ec2.DescribeImagesWithContext(ctx, &ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("name"),
			Values: []*string{aws.String("ami-ubuntu-*")},
		}},
	})
	if err != nil {
		return "", fmt.Errorf("e2e harness: DescribeImages: %w", err)
	}
	if len(out.Images) == 0 || out.Images[0].ImageId == nil {
		return "", errors.New("e2e harness: no ami-ubuntu-* images registered in the cluster")
	}
	w.ami = aws.StringValue(out.Images[0].ImageId)
	return w.ami, nil
}

// resolveInstanceType returns the witness instance type, caching the result.
// Falls back to the smallest registered type (by memory, then vCPUs) so the
// choice doesn't depend on naming conventions that shift between releases.
func (w *Witness) resolveInstanceType(ctx context.Context) (string, error) {
	if w.instanceType != "" {
		return w.instanceType, nil
	}
	out, err := w.ec2.DescribeInstanceTypesWithContext(ctx, &ec2.DescribeInstanceTypesInput{})
	if err != nil {
		return "", fmt.Errorf("e2e harness: DescribeInstanceTypes: %w", err)
	}
	if len(out.InstanceTypes) == 0 {
		return "", errors.New("e2e harness: cluster registered no instance types")
	}
	sort.Slice(out.InstanceTypes, func(i, j int) bool {
		a, b := out.InstanceTypes[i], out.InstanceTypes[j]
		var aMem, bMem int64
		if a.MemoryInfo != nil {
			aMem = aws.Int64Value(a.MemoryInfo.SizeInMiB)
		}
		if b.MemoryInfo != nil {
			bMem = aws.Int64Value(b.MemoryInfo.SizeInMiB)
		}
		if aMem != bMem {
			return aMem < bMem
		}
		var aCPU, bCPU int64
		if a.VCpuInfo != nil {
			aCPU = aws.Int64Value(a.VCpuInfo.DefaultVCpus)
		}
		if b.VCpuInfo != nil {
			bCPU = aws.Int64Value(b.VCpuInfo.DefaultVCpus)
		}
		return aCPU < bCPU
	})
	w.instanceType = aws.StringValue(out.InstanceTypes[0].InstanceType)
	if w.instanceType == "" {
		return "", errors.New("e2e harness: smallest instance type has empty name")
	}
	return w.instanceType, nil
}

func (w *Witness) waitForRunning(ctx context.Context, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	const interval = 2 * time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		out, err := w.ec2.DescribeInstancesWithContext(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(id)},
		})
		if err == nil {
			for _, r := range out.Reservations {
				for _, inst := range r.Instances {
					if inst.State != nil && aws.StringValue(inst.State.Name) == "running" {
						return nil
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("e2e harness: instance %s did not reach running within %s", id, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// findHost walks cluster nodes looking for the QEMU process that owns instanceID
// (the instance ID appears in -pidfile, -qmp, and -name on the QEMU command line).
func (w *Witness) findHost(ctx context.Context, instanceID string) (Node, error) {
	cmd := fmt.Sprintf("ps auxw | grep %s | grep qemu-system | grep -v grep", ShellQuote(instanceID))
	// EC2 "running" state is reported before QEMU is forked; give it a window.
	deadline := time.Now().Add(30 * time.Second)
	const interval = 1 * time.Second
	for {
		for _, n := range w.cluster.Nodes {
			out, err := w.ssh.Run(ctx, n, cmd)
			if err != nil {
				continue // grep exits 1 on no match; try next node
			}
			if strings.Contains(string(out), instanceID) {
				return n, nil
			}
		}
		if time.Now().After(deadline) {
			return Node{}, fmt.Errorf("e2e harness: could not locate QEMU host for %s across %d nodes", instanceID, len(w.cluster.Nodes))
		}
		select {
		case <-ctx.Done():
			return Node{}, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// resolvePublicIP queries the cluster's EC2 gateway for the instance's
// auto-allocated public IP. PrepareRunInstances populates this when the
// launch path's default subnet has MapPublicIpOnLaunch=true (the spx admin
// init default). The runner reaches the IP via the cluster's external pool
// — the same WAN subnet as DDIL_NODES — so no host relay is required and
// the path survives Scenario C peer-only iptables partitions.
func (w *Witness) resolvePublicIP(ctx context.Context, instanceID string) (string, error) {
	deadline := time.Now().Add(30 * time.Second)
	const interval = 1 * time.Second
	for {
		out, err := w.ec2.DescribeInstancesWithContext(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		if err == nil {
			for _, r := range out.Reservations {
				for _, inst := range r.Instances {
					if ip := aws.StringValue(inst.PublicIpAddress); ip != "" {
						return ip, nil
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("e2e harness: instance %s has no PublicIpAddress after %s (default subnet MapPublicIpOnLaunch may be off)", instanceID, 30*time.Second)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (w *Witness) terminate(ctx context.Context, id string) error {
	_, err := w.ec2.TerminateInstancesWithContext(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(id)},
	})
	if err != nil {
		return fmt.Errorf("e2e harness: TerminateInstances %s: %w", id, err)
	}
	return nil
}

// readCounter tunnels SSH through host to publicIP:22 and reads /var/lib/counter.
// The runner can't reach the EIP directly; the hosting node jumps via OVN's gateway.
func readCounter(ctx context.Context, w *Witness, host Node, publicIP string) (int, error) {
	hostClient, err := dialHost(ctx, host, w.cluster.SSHUser, w.hostSigner)
	if err != nil {
		return 0, err
	}
	defer func() { _ = hostClient.Close() }()

	guestAddr := net.JoinHostPort(publicIP, "22")
	tunnel, err := hostClient.DialContext(ctx, "tcp", guestAddr)
	if err != nil {
		return 0, fmt.Errorf("e2e harness: tunnel %s → %s: %w", host.Name, guestAddr, err)
	}

	guestCfg := &ssh.ClientConfig{
		User:            w.guestUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(w.guestSigner)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // ephemeral test VM
		Timeout:         10 * time.Second,
	}
	c, chans, reqs, err := ssh.NewClientConn(tunnel, guestAddr, guestCfg)
	if err != nil {
		_ = tunnel.Close()
		return 0, fmt.Errorf("e2e harness: guest ssh handshake on %s: %w", guestAddr, err)
	}
	guestClient := ssh.NewClient(c, chans, reqs)
	defer func() { _ = guestClient.Close() }()

	session, err := guestClient.NewSession()
	if err != nil {
		return 0, fmt.Errorf("e2e harness: guest ssh session: %w", err)
	}
	defer func() { _ = session.Close() }()

	raw, err := session.CombinedOutput("cat /var/lib/counter")
	if err != nil {
		return 0, fmt.Errorf("e2e harness: read /var/lib/counter: %w (output: %s)", err, strings.TrimSpace(string(raw)))
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("e2e harness: parse /var/lib/counter %q: %w", s, err)
	}
	return n, nil
}

// awaitBaseline polls readCounter until the guest returns a parseable counter
// value. Retries on transient transport errors (sshd not yet up, EIP rule not
// yet landed, counter file not yet created). Auth and parse errors fail fast.
func awaitBaseline(ctx context.Context, w *Witness, host Node, publicIP string, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	const interval = 2 * time.Second
	var lastErr error
	for {
		n, err := readCounter(ctx, w, host, publicIP)
		if err == nil {
			return n, nil
		}
		lastErr = err
		msg := strings.ToLower(err.Error())
		transient := strings.Contains(msg, "connect failed") ||
			strings.Contains(msg, "connection refused") ||
			strings.Contains(msg, "connection reset") ||
			strings.Contains(msg, "eof") ||
			strings.Contains(msg, "i/o timeout") ||
			strings.Contains(msg, "no route to host") ||
			strings.Contains(msg, "no such file or directory")
		if !transient {
			return 0, err
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("e2e harness: %s baseline not ready after %s: %w", publicIP, timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func dialHost(ctx context.Context, node Node, user string, signer ssh.Signer) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // tofu-cluster test hosts
		Timeout:         10 * time.Second,
	}
	d := net.Dialer{Timeout: cfg.Timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(node.Addr, "22"))
	if err != nil {
		return nil, fmt.Errorf("e2e harness: dial host %s: %w", node.Name, err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, node.Addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("e2e harness: host ssh handshake %s: %w", node.Name, err)
	}
	return ssh.NewClient(c, chans, reqs), nil
}

func loadSigner(path string) (ssh.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	s, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

func envDefault(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
