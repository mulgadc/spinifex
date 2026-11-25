//go:build e2e

package multinode

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	vpceObjectKey = "vpce-multinode-marker"
	vpceCred      = "/home/ubuntu/.vpce-allow.curl"

	// vpceFlipBound is how soon after the request every node's datapath must
	// reflect an endpoint create, modify or delete.
	vpceFlipBound = 10 * time.Second
)

// vpceTarget is one private guest and the gate it is timed against, chosen on
// another host when the guest's host is known among the entries.
type vpceTarget struct {
	InstanceID string
	PrivateIP  string
	Node       string
	Gate       string
	GateRemote bool
}

// runMultinodeVPCGatewayEndpoint proves a private-subnet guest on every node
// reaches every node's gate through one S3 gateway endpoint, and times each
// create, modify and delete to the datapath change on each node.
func runMultinodeVPCGatewayEndpoint(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — S3 gateway VPC endpoint across hosts, timed per node")
	c := fix.AWS

	harness.Step(t, "wait for the S3 gateway endpoint service to be offered")
	svc := harness.WaitForS3EndpointService(t, c)
	region := harness.S3EndpointRegion(svc)
	plID, entries := harness.S3PrefixList(t, c, svc)
	port := harness.S3GatePort()
	harness.Detail(t, "service", svc, "prefix_list", plID, "entries", strings.Join(entries, ","), "gate_port", port)
	require.Lenf(t, entries, len(fix.Cluster.Nodes),
		"the prefix list must name one gate per node (%d nodes), got %v", len(fix.Cluster.Nodes), entries)

	v := spreadSetupVPC(t, fix)
	rtOut, err := c.EC2.CreateRouteTable(&ec2.CreateRouteTableInput{VpcId: aws.String(v.VPCID)})
	require.NoError(t, err, "create-route-table (private)")
	privRT := aws.StringValue(rtOut.RouteTable.RouteTableId)
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{RouteTableId: aws.String(privRT)})
	})
	assoc, err := c.EC2.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(privRT), SubnetId: aws.String(v.PrivSubnetID),
	})
	require.NoError(t, err, "associate-route-table %s <- %s", privRT, v.PrivSubnetID)
	assocID := aws.StringValue(assoc.AssociationId)
	t.Cleanup(func() {
		_, _ = c.EC2.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{AssociationId: aws.String(assocID)})
	})

	b := spreadSetupBastion(t, fix, v)
	p := spreadSetupPrivateTrio(t, fix, b)
	require.Equalf(t, len(fix.Cluster.Nodes), uniqueCount(p.HostingNodes),
		"the endpoint must be timed on every node, but the guests landed on %v", p.HostingNodes)
	bastion := harness.SSHTarget{User: "ubuntu", Host: b.PublicIP, Port: 22, KeyPath: b.KeyPath}

	suffix := time.Now().UnixNano()
	bucket := fmt.Sprintf("vpce-mn-%d", suffix)
	principal := harness.CreateS3Principal(t, c, fmt.Sprintf("vpce-mn-%d", suffix), harness.S3BucketStatements(bucket))
	harness.CreateS3BucketWithObject(t, entries[0], principal, bucket, vpceObjectKey)

	targets := vpceTargets(fix, p, entries)
	for _, tg := range targets {
		if _, werr := harness.GuestExecViaBastion(bastion, tg.PrivateIP, harness.GuestS3CurlConfigCmd(vpceCred, principal), 30*time.Second); werr != nil {
			t.Fatalf("writing the curl config into %s: %v", tg.InstanceID, werr)
		}
		harness.Detail(t, "instance", tg.InstanceID, "node", tg.Node, "timed_gate", tg.Gate, "gate_remote", tg.GateRemote)
	}
	probe := func(ip, gate string) (harness.GuestCurl, error) {
		return harness.ProbeGuestS3(bastion, ip, harness.GuestS3ListCmd(vpceCred, region, gate, port, bucket))
	}
	flip := func(t *testing.T, phase, want string, act func() error) bool {
		return vpceTimeFlip(t, bastion, targets, phase, want, vpceFlipScript(want, region, port, bucket), act)
	}

	t.Run("NoEndpointNoPath", func(t *testing.T) {
		for _, tg := range targets {
			g, err := probe(tg.PrivateIP, tg.Gate)
			require.NoError(t, err, "probe from %s", tg.InstanceID)
			assert.Truef(t, g.TimedOut(), "%s on %s reached %s:%d with no endpoint: %s", tg.InstanceID, tg.Node, tg.Gate, port, g)
		}
	})

	var epID string
	allSaw := false
	t.Run("CreateTimed", func(st *testing.T) {
		allSaw = flip(st, "create", "up", func() error {
			out, err := c.EC2.CreateVpcEndpoint(&ec2.CreateVpcEndpointInput{
				VpcId: aws.String(v.VPCID), ServiceName: aws.String(svc),
				VpcEndpointType: aws.String(ec2.VpcEndpointTypeGateway), RouteTableIds: []*string{aws.String(privRT)},
			})
			if err != nil {
				return err
			}
			if out.VpcEndpoint == nil {
				return fmt.Errorf("create-vpc-endpoint returned no VpcEndpoint")
			}
			epID = aws.StringValue(out.VpcEndpoint.VpcEndpointId)
			t.Cleanup(func() {
				if derr := harness.DeleteVpcEndpointWait(c, epID, 2*time.Minute); derr != nil {
					t.Logf("cleanup: %v", derr)
				}
			})
			return nil
		})
	})
	if epID == "" || !allSaw {
		t.Fatalf("CreateTimed did not open the path on every node; the later stages need it")
	}
	harness.Detail(t, "vpc_endpoint", epID)

	// Every guest reaches every gate; with one gate per node, that includes
	// the gate of every host the guest is not on.
	t.Run("CrossHostGate", func(t *testing.T) {
		for _, tg := range targets {
			for _, gate := range entries {
				g, err := probe(tg.PrivateIP, gate)
				require.NoError(t, err, "probe %s from %s", gate, tg.InstanceID)
				assert.Truef(t, g.Code == "200" && strings.Contains(g.Body, vpceObjectKey),
					"%s on %s could not list the bucket through %s: %s", tg.InstanceID, tg.Node, gate, g)
			}
		}
	})

	t.Run("ModifyRemoveTimed", func(t *testing.T) {
		flip(t, "modify_remove", "down", func() error {
			_, err := c.EC2.ModifyVpcEndpoint(&ec2.ModifyVpcEndpointInput{
				VpcEndpointId: aws.String(epID), RemoveRouteTableIds: []*string{aws.String(privRT)},
			})
			return err
		})
	})

	t.Run("ModifyAddTimed", func(t *testing.T) {
		flip(t, "modify_add", "up", func() error {
			_, err := c.EC2.ModifyVpcEndpoint(&ec2.ModifyVpcEndpointInput{
				VpcEndpointId: aws.String(epID), AddRouteTableIds: []*string{aws.String(privRT)},
			})
			return err
		})
	})

	t.Run("DeleteTimed", func(t *testing.T) {
		flip(t, "delete", "down", func() error {
			out, err := c.EC2.DeleteVpcEndpoints(&ec2.DeleteVpcEndpointsInput{VpcEndpointIds: []*string{aws.String(epID)}})
			if err == nil && len(out.Unsuccessful) > 0 {
				return fmt.Errorf("delete-vpc-endpoints %s unsuccessful: %v", epID, out.Unsuccessful)
			}
			return err
		})
		require.Nil(t, harness.EndpointRoute(t, c, privRT, plID), "%s still shows the endpoint route after delete", privRT)
	})
}

// vpceTargets pairs each private guest with a gate on another host when the
// guest's host address is among the entries, else with the first entry.
func vpceTargets(fix *Fixture, p spreadPrivate, entries []string) []vpceTarget {
	addrOf := map[string]string{}
	for _, n := range fix.Cluster.Nodes {
		addrOf[n.Name] = n.Addr
	}
	out := make([]vpceTarget, 0, len(p.InstanceIDs))
	for i, id := range p.InstanceIDs {
		tg := vpceTarget{InstanceID: id, PrivateIP: p.PrivateIPs[i], Node: p.HostingNodes[i], Gate: entries[0]}
		host := addrOf[tg.Node]
		hostListed := false
		for _, e := range entries {
			hostListed = hostListed || e == host
		}
		if hostListed {
			for _, e := range entries {
				if e != host {
					tg.Gate, tg.GateRemote = e, true
					break
				}
			}
		}
		out = append(out, tg)
	}
	return out
}

// vpceFlipScript waits until three successive probes show the opposite of
// want and prints "ready". After a "go" line on stdin it probes until want
// ("up": HTTP 200, "down": no HTTP status), prints "seen", and prints "held"
// once three more probes agree, or "flapped" when one does not.
func vpceFlipScript(want, region string, port int, bucket string) string {
	other := "up"
	if want == "up" {
		other = "down"
	}
	return fmt.Sprintf(`probe() { curl -sk -K %[1]s --aws-sigv4 %[2]s --connect-timeout 1 --max-time 3 -o /dev/null -w '%%{http_code}' "https://$GATE:%[3]d/%[4]s?list-type=2"; }
is() { case "$1:$2" in up:200|down:000) return 0;; esac; return 1; }
t0=$(date +%%s); n=0
while [ $n -lt 3 ]; do
  code=$(probe)
  if is %[6]s "$code"; then n=$((n+1)); else n=0; fi
  if [ $(($(date +%%s)-t0)) -ge 60 ]; then echo "unsettled $code"; exit 0; fi
  sleep 0.2
done
echo ready
read -r _
t0=$(date +%%s)
until is %[5]s "$(probe)"; do
  if [ $(($(date +%%s)-t0)) -ge 60 ]; then echo "expired"; exit 0; fi
  sleep 0.2
done
echo seen
for i in 1 2 3; do
  sleep 0.5
  code=$(probe)
  is %[5]s "$code" || { echo "flapped $code"; exit 0; }
done
echo held`, harness.ShellQuote(vpceCred), harness.ShellQuote("aws:amz:"+region+":s3"), port, bucket, want, other)
}

// vpceLine is one line of a flip script's output and when the runner read it.
type vpceLine struct {
	text string
	at   time.Time
}

// vpceWatch is one guest's running flip script.
type vpceWatch struct {
	target vpceTarget
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *bytes.Buffer
	lines  chan vpceLine
}

// vpceTimeFlip runs act once every target shows the opposite state, then lets
// the guests probe. It asserts each guest's first probe to see the change was
// read within vpceFlipBound of the request and that the change held.
func vpceTimeFlip(t *testing.T, bastion harness.SSHTarget, targets []vpceTarget, phase, want, script string, act func() error) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	watches := make([]*vpceWatch, 0, len(targets))
	defer func() {
		cancel()
		for _, w := range watches {
			_ = w.cmd.Wait()
		}
	}()
	for _, tg := range targets {
		cmd := exec.CommandContext(ctx, "ssh", harness.BastionHopArgs(bastion, tg.PrivateIP, "GATE="+tg.Gate+"; "+script)...)
		cmd.WaitDelay = 5 * time.Second
		stdout, err := cmd.StdoutPipe()
		require.NoError(t, err, "stdout pipe for %s", tg.InstanceID)
		stdin, err := cmd.StdinPipe()
		require.NoError(t, err, "stdin pipe for %s", tg.InstanceID)
		w := &vpceWatch{target: tg, cmd: cmd, stdin: stdin, stderr: &bytes.Buffer{}, lines: make(chan vpceLine, 16)}
		cmd.Stderr = w.stderr
		require.NoError(t, cmd.Start(), "start flip script on %s", tg.InstanceID)
		watches = append(watches, w)
		go func() {
			sc := bufio.NewScanner(stdout)
			for sc.Scan() {
				w.lines <- vpceLine{text: strings.TrimSpace(sc.Text()), at: time.Now()}
			}
			close(w.lines)
		}()
	}
	for _, w := range watches {
		line, ok := vpceNextLine(w, 90*time.Second)
		require.Truef(t, ok && line.text == "ready", "%s on %s never settled before %s (got %q): %s",
			w.target.InstanceID, w.target.Node, phase, line.text, strings.TrimSpace(w.stderr.String()))
	}

	// Probing starts only once the request has returned, so no probe can
	// complete before the change was asked for.
	start := time.Now()
	require.NoError(t, act(), "%s request", phase)
	apiMS := time.Since(start).Milliseconds()
	for _, w := range watches {
		_, err := io.WriteString(w.stdin, "go\n")
		require.NoError(t, err, "release flip script on %s", w.target.InstanceID)
	}

	allSaw := true
	for _, w := range watches {
		line, ok := vpceNextLine(w, 90*time.Second)
		if !ok || line.text != "seen" {
			allSaw = false
			t.Errorf("%s on %s never saw %s after %s (got %q): %s", w.target.InstanceID, w.target.Node, want, phase,
				line.text, strings.TrimSpace(w.stderr.String()))
			continue
		}
		flipMS := line.at.Sub(start).Milliseconds()
		harness.Detail(t, "phase", phase, "instance", w.target.InstanceID, "node", w.target.Node,
			"gate", w.target.Gate, "api_ms", apiMS, "flip_ms", flipMS, "bound_ms", vpceFlipBound.Milliseconds())
		assert.LessOrEqualf(t, flipMS, vpceFlipBound.Milliseconds(),
			"%s on %s took %dms to see %s after %s", w.target.InstanceID, w.target.Node, flipMS, want, phase)
		if line, ok := vpceNextLine(w, 30*time.Second); !ok || line.text != "held" {
			allSaw = false
			t.Errorf("%s on %s did not hold %s after %s (got %q)", w.target.InstanceID, w.target.Node, want, phase, line.text)
		}
	}
	return allSaw
}

// vpceNextLine returns w's next output line, or false if none arrives in budget.
func vpceNextLine(w *vpceWatch, budget time.Duration) (vpceLine, bool) {
	select {
	case line, ok := <-w.lines:
		return line, ok
	case <-time.After(budget):
		return vpceLine{}, false
	}
}
