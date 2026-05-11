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

// NewWitness constructs a Witness from environment-supplied settings:
//
//   - AWS_REGION (required) — passed to the AWS SDK client.
//   - DDIL_WITNESS_AMI (optional) — specific AMI ID; if unset, an
//     ami-ubuntu-* image is discovered at launch time.
//   - DDIL_WITNESS_INSTANCE_TYPE (optional) — explicit instance type; if
//     unset, the smallest registered type (by memory, then vCPUs) is
//     auto-discovered at launch time. Auto-discovery beats hard-coding
//     `t2.micro` because tofu clusters seed `*.nano` variants under
//     names that change between releases.
//   - DDIL_WITNESS_KEY_NAME (default spinifex-key) — EC2 key pair name
//     the daemon injects via cloud-init.
//   - DDIL_GUEST_SSH_USER (default ec2-user) — user for guest SSH; matches
//     the default account on the cluster's ami-ubuntu-* images (shell E2E
//     uses the same login at tests/e2e/run-multinode-e2e.sh:650).
//   - DDIL_GUEST_SSH_KEY (required) — private key for guest SSH; must
//     pair with the public material registered under DDIL_WITNESS_KEY_NAME
//     so authorized_keys on the cloud-init guest accepts it.
//
// Credentials for the SDK come from the default chain (AWS_ACCESS_KEY_ID/
// AWS_SECRET_ACCESS_KEY or shared profile), matching the tofu-cluster
// convention used by the shell E2E suites.
func NewWitness(cluster *Cluster, transport SSH) (*Witness, error) {
	region := os.Getenv("AWS_REGION")
	if region == "" {
		return nil, errors.New("ddil harness: NewWitness requires AWS_REGION")
	}
	if len(cluster.Nodes) == 0 {
		return nil, errors.New("ddil harness: NewWitness requires a non-empty cluster")
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
		return nil, fmt.Errorf("ddil harness: aws session: %w", err)
	}

	hostSigner, err := loadSigner(cluster.SSHKeyPath)
	if err != nil {
		return nil, fmt.Errorf("ddil harness: host ssh key: %w", err)
	}

	guestKeyPath := os.Getenv("DDIL_GUEST_SSH_KEY")
	if guestKeyPath == "" {
		return nil, errors.New("ddil harness: NewWitness requires DDIL_GUEST_SSH_KEY (path to the private key paired with DDIL_WITNESS_KEY_NAME's registered material)")
	}
	guestSigner, err := loadSigner(guestKeyPath)
	if err != nil {
		return nil, fmt.Errorf("ddil harness: guest ssh key: %w", err)
	}

	return &Witness{
		ec2:          ec2.New(sess),
		cluster:      cluster,
		ssh:          transport,
		hostSigner:   hostSigner,
		guestSigner:  guestSigner,
		guestUser:    envDefault("DDIL_GUEST_SSH_USER", "ec2-user"),
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
			return nil, fmt.Errorf("ddil harness: RunInstances: %w", err)
		}
		if len(out.Instances) == 0 || out.Instances[0].InstanceId == nil {
			return nil, errors.New("ddil harness: RunInstances returned no instance")
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
			if err := awaitGuestSSH(ctx, w, placed, publicIP, 90*time.Second); err != nil {
				_ = w.terminate(ctx, id)
				return nil, fmt.Errorf("ddil harness: witness sshd ready: %w", err)
			}
			baseline, err := readCounter(ctx, w, placed, publicIP)
			if err != nil {
				// Counter service may still be seeding /var/lib/counter on
				// first ExecStartPre after sshd came up. Treat as fatal so
				// scenarios don't silently skip the progress assertion.
				_ = w.terminate(ctx, id)
				return nil, fmt.Errorf("ddil harness: witness baseline read: %w", err)
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
	return nil, fmt.Errorf("ddil harness: LaunchWitnessVM: %w after %d attempts", lastErr, maxPlacementAttempts)
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
		t.Fatalf("ddil harness: read witness counter on %s (%s): %v", v.HostNode.Name, v.InstanceID, err)
	}
	if current <= v.BaselineCounter {
		t.Fatalf("ddil harness: witness %s on %s did not progress: baseline=%d current=%d",
			v.InstanceID, v.HostNode.Name, v.BaselineCounter, current)
	}
	t.Logf("witness %s on %s progressed %d → %d", v.InstanceID, v.HostNode.Name, v.BaselineCounter, current)
}

// --- internals ------------------------------------------------------------

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
		return "", fmt.Errorf("ddil harness: DescribeImages: %w", err)
	}
	if len(out.Images) == 0 || out.Images[0].ImageId == nil {
		return "", errors.New("ddil harness: no ami-ubuntu-* images registered in the cluster")
	}
	w.ami = aws.StringValue(out.Images[0].ImageId)
	return w.ami, nil
}

// resolveInstanceType returns the witness instance type, caching the result.
// When DDIL_WITNESS_INSTANCE_TYPE is set it is honoured verbatim; otherwise
// DescribeInstanceTypes is consulted and the smallest registered type — by
// memory first, vCPUs as tiebreaker — is selected. The sort key is
// quantitative on purpose: naming conventions (`t2.nano`, `c6i.large`, …)
// shift between cluster releases, but smallest-by-resource always picks the
// cheapest valid launch target.
func (w *Witness) resolveInstanceType(ctx context.Context) (string, error) {
	if w.instanceType != "" {
		return w.instanceType, nil
	}
	out, err := w.ec2.DescribeInstanceTypesWithContext(ctx, &ec2.DescribeInstanceTypesInput{})
	if err != nil {
		return "", fmt.Errorf("ddil harness: DescribeInstanceTypes: %w", err)
	}
	if len(out.InstanceTypes) == 0 {
		return "", errors.New("ddil harness: cluster registered no instance types")
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
		return "", errors.New("ddil harness: smallest instance type has empty name")
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
			return fmt.Errorf("ddil harness: instance %s did not reach running within %s", id, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// findHost walks every cluster node looking for the QEMU process that owns
// instanceID. The daemon embeds the instance ID in `-pidfile`, `-qmp`, and
// `-name guest=` on the QEMU command line, so grepping for the ID identifies
// exactly one process per cluster regardless of net mode (user-hostfwd vs
// tap-mode bridge).
func (w *Witness) findHost(ctx context.Context, instanceID string) (Node, error) {
	cmd := fmt.Sprintf("ps auxw | grep %s | grep qemu-system | grep -v grep", shellQuote(instanceID))
	// Give the daemon a short window to actually spawn QEMU after the
	// EC2 state flip to running, since /aws/ec2 reports "running" before
	// the daemon has finished forking the process on some nodes.
	deadline := time.Now().Add(30 * time.Second)
	const interval = 1 * time.Second
	for {
		for _, n := range w.cluster.Nodes {
			out, err := w.ssh.Run(ctx, n, cmd)
			if err != nil {
				// `grep | grep` exits 1 when no match — our SSH wrapper treats
				// that as error; try the next node rather than bailing.
				continue
			}
			if strings.Contains(string(out), instanceID) {
				return n, nil
			}
		}
		if time.Now().After(deadline) {
			return Node{}, fmt.Errorf("ddil harness: could not locate QEMU host for %s across %d nodes", instanceID, len(w.cluster.Nodes))
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
			return "", fmt.Errorf("ddil harness: instance %s has no PublicIpAddress after %s (default subnet MapPublicIpOnLaunch may be off)", instanceID, 30*time.Second)
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
		return fmt.Errorf("ddil harness: TerminateInstances %s: %w", id, err)
	}
	return nil
}

// readCounter tunnels SSH through host to publicIP:22 and reads
// /var/lib/counter. The cluster's external pool (MapPublicIpOnLaunch) sits
// on an OVN-managed subnet that isn't routable from the off-cluster runner;
// the host node, however, reaches it via OVN's gateway router. Using the
// VM's hosting node as the jump keeps the relay inside the cluster.
func readCounter(ctx context.Context, w *Witness, host Node, publicIP string) (int, error) {
	hostClient, err := dialHost(ctx, host, w.cluster.SSHUser, w.hostSigner)
	if err != nil {
		return 0, err
	}
	defer func() { _ = hostClient.Close() }()

	guestAddr := net.JoinHostPort(publicIP, "22")
	tunnel, err := hostClient.DialContext(ctx, "tcp", guestAddr)
	if err != nil {
		return 0, fmt.Errorf("ddil harness: tunnel %s → %s: %w", host.Name, guestAddr, err)
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
		return 0, fmt.Errorf("ddil harness: guest ssh handshake on %s: %w", guestAddr, err)
	}
	guestClient := ssh.NewClient(c, chans, reqs)
	defer func() { _ = guestClient.Close() }()

	session, err := guestClient.NewSession()
	if err != nil {
		return 0, fmt.Errorf("ddil harness: guest ssh session: %w", err)
	}
	defer func() { _ = session.Close() }()

	raw, err := session.CombinedOutput("cat /var/lib/counter")
	if err != nil {
		return 0, fmt.Errorf("ddil harness: read /var/lib/counter: %w (output: %s)", err, strings.TrimSpace(string(raw)))
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("ddil harness: parse /var/lib/counter %q: %w", s, err)
	}
	return n, nil
}

// awaitGuestSSH polls the host-tunnelled SSH handshake to publicIP:22 until
// it succeeds or timeout expires. Cloud-init brings the witness AMI up in
// 30–60s on tofu-cluster hardware; the OVN dnat_and_snat rule for the EIP
// is also written asynchronously to RunInstances, so the tunnel can land
// before either layer is ready. We retry on the transport errors that
// indicate "guest not yet listening" (Connection refused / reset / EOF /
// io timeout) and fail fast on auth errors so a misconfigured key surfaces
// immediately.
func awaitGuestSSH(ctx context.Context, w *Witness, host Node, publicIP string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	const interval = 2 * time.Second
	var lastErr error
	for {
		err := probeGuestSSH(ctx, w, host, publicIP)
		if err == nil {
			return nil
		}
		lastErr = err
		msg := strings.ToLower(err.Error())
		// "connect failed" covers x/crypto/ssh's
		//   `ssh: rejected: connect failed (Connection refused)`
		// which fires when the SSH jump host's DialContext fails — the
		// canonical signal that the guest hasn't bound :22 yet.
		transient := strings.Contains(msg, "connect failed") ||
			strings.Contains(msg, "connection refused") ||
			strings.Contains(msg, "connection reset") ||
			strings.Contains(msg, "eof") ||
			strings.Contains(msg, "i/o timeout") ||
			strings.Contains(msg, "no route to host")
		if !transient {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ddil harness: %s sshd not ready after %s: %w", publicIP, timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// probeGuestSSH opens, handshakes, and closes a single SSH session through
// host to publicIP:22 — used as the readiness check by awaitGuestSSH.
func probeGuestSSH(ctx context.Context, w *Witness, host Node, publicIP string) error {
	hostClient, err := dialHost(ctx, host, w.cluster.SSHUser, w.hostSigner)
	if err != nil {
		return err
	}
	defer func() { _ = hostClient.Close() }()

	guestAddr := net.JoinHostPort(publicIP, "22")
	tunnel, err := hostClient.DialContext(ctx, "tcp", guestAddr)
	if err != nil {
		return fmt.Errorf("ddil harness: tunnel %s → %s: %w", host.Name, guestAddr, err)
	}

	guestCfg := &ssh.ClientConfig{
		User:            w.guestUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(w.guestSigner)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // ephemeral test VM
		Timeout:         5 * time.Second,
	}
	c, chans, reqs, err := ssh.NewClientConn(tunnel, guestAddr, guestCfg)
	if err != nil {
		_ = tunnel.Close()
		return fmt.Errorf("ddil harness: guest ssh handshake on %s: %w", guestAddr, err)
	}
	_ = ssh.NewClient(c, chans, reqs).Close()
	return nil
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
		return nil, fmt.Errorf("ddil harness: dial host %s: %w", node.Name, err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, node.Addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ddil harness: host ssh handshake %s: %w", node.Name, err)
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
