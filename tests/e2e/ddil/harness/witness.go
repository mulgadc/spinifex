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
	"regexp"
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
//   - DDIL_WITNESS_KEY_NAME (default spinifex-key) — EC2 key pair name.
//   - DDIL_GUEST_SSH_USER (default ubuntu) — user for guest SSH.
//   - DDIL_GUEST_SSH_KEY (default cluster.SSHKeyPath) — private key for
//     guest SSH; may point at the same key baked into the AMI.
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

	guestKeyPath := envDefault("DDIL_GUEST_SSH_KEY", cluster.SSHKeyPath)
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
	HostNode        Node // cluster node the VM's QEMU process lives on
	SSHPort         int  // QEMU hostfwd port on HostNode mapped to guest :22
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

		placed, port, err := w.findHost(ctx, id)
		if err != nil {
			_ = w.terminate(ctx, id)
			return nil, err
		}

		if placed.Index == host.Index {
			baseline, err := readCounterViaTunnel(ctx, w, placed, port)
			if err != nil {
				// The counter service may not have started yet on a freshly
				// booted guest; a zero baseline is the expected steady state
				// since the service seeds /var/lib/counter with 0 on first
				// ExecStartPre. Treat a connection failure as fatal so
				// scenarios don't silently skip the progress assertion.
				_ = w.terminate(ctx, id)
				return nil, fmt.Errorf("ddil harness: witness baseline read: %w", err)
			}
			return &WitnessVM{
				InstanceID:      id,
				HostNode:        placed,
				SSHPort:         port,
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

// ReadCounter SSHes into the guest via its host's QEMU hostfwd port and
// returns the current /var/lib/counter value.
func (v *WitnessVM) ReadCounter(ctx context.Context) (int, error) {
	return readCounterViaTunnel(ctx, v.w, v.HostNode, v.SSHPort)
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

var hostfwdPortRE = regexp.MustCompile(`hostfwd=tcp:[^:]*:(\d+)-:22`)

// findHost walks every cluster node looking for the QEMU process that owns
// instanceID. Returns the hosting node and the SSH hostfwd port extracted
// from its command line. Matches the shell helper pattern in
// run-multinode-e2e.sh:163-180.
func (w *Witness) findHost(ctx context.Context, instanceID string) (Node, int, error) {
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
			m := hostfwdPortRE.FindStringSubmatch(string(out))
			if len(m) == 2 {
				port, err := strconv.Atoi(m[1])
				if err == nil {
					return n, port, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return Node{}, 0, fmt.Errorf("ddil harness: could not locate QEMU host for %s across %d nodes", instanceID, len(w.cluster.Nodes))
		}
		select {
		case <-ctx.Done():
			return Node{}, 0, ctx.Err()
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

// readCounterViaTunnel opens an SSH connection to host, tunnels a TCP
// channel to 127.0.0.1:port (QEMU's guest hostfwd), runs a fresh SSH
// handshake over that channel using the guest credentials, and reads
// /var/lib/counter.
//
// The hostfwd target is 127.0.0.1 because the daemon binds QEMU's user-mode
// networking to loopback; the orchestrator cannot reach it directly without
// the host SSH relay.
func readCounterViaTunnel(ctx context.Context, w *Witness, host Node, port int) (int, error) {
	hostClient, err := dialHost(ctx, host, w.cluster.SSHUser, w.hostSigner)
	if err != nil {
		return 0, err
	}
	defer func() { _ = hostClient.Close() }()

	guestAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
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
	conn, chans, reqs, err := ssh.NewClientConn(tunnel, guestAddr, guestCfg)
	if err != nil {
		_ = tunnel.Close()
		return 0, fmt.Errorf("ddil harness: guest ssh handshake on %s: %w", guestAddr, err)
	}
	guestClient := ssh.NewClient(conn, chans, reqs)
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
