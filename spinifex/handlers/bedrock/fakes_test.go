package handlers_bedrock

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/handlers/sysinstance"
	handlers_systemvpc "github.com/mulgadc/spinifex/spinifex/handlers/systemvpc"
)

// fakeSystemVPC is the whole EC2 VPC family as the system-VPC builder sees
// it: every describe is empty and every create succeeds, mirroring RDS's own
// launch_test.go fixture of the same name (the systemvpc package's own rules
// are covered in its package tests, not re-verified here).
type fakeSystemVPC struct {
	seq        int
	igwCreated bool
}

var (
	_ handlers_systemvpc.VPCProvisioner        = (*fakeSystemVPC)(nil)
	_ handlers_systemvpc.RouteTableProvisioner = (*fakeSystemVPC)(nil)
	_ handlers_systemvpc.NATGatewayProvisioner = (*fakeSystemVPC)(nil)
	_ handlers_systemvpc.EIPProvisioner        = (*fakeSystemVPC)(nil)
	_ handlers_systemvpc.IGWProvisioner        = (*fakeSystemVPC)(nil)
)

func (f *fakeSystemVPC) id(prefix string) *string {
	f.seq++
	return aws.String(fmt.Sprintf("%s-%04d", prefix, f.seq))
}

func (f *fakeSystemVPC) deps() handlers_systemvpc.Deps {
	return handlers_systemvpc.Deps{VPC: f, IGW: f, RT: f, NGW: f, EIP: f}
}

func (f *fakeSystemVPC) CreateVpc(context.Context, *ec2.CreateVpcInput, string) (*ec2.CreateVpcOutput, error) {
	return &ec2.CreateVpcOutput{Vpc: &ec2.Vpc{VpcId: f.id("vpc")}}, nil
}

func (f *fakeSystemVPC) DeleteVpc(context.Context, *ec2.DeleteVpcInput, string) (*ec2.DeleteVpcOutput, error) {
	return &ec2.DeleteVpcOutput{}, nil
}

func (f *fakeSystemVPC) DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, string) (*ec2.DescribeVpcsOutput, error) {
	return &ec2.DescribeVpcsOutput{}, nil
}

func (f *fakeSystemVPC) CreateSubnet(_ context.Context, in *ec2.CreateSubnetInput, _ string) (*ec2.CreateSubnetOutput, error) {
	return &ec2.CreateSubnetOutput{Subnet: &ec2.Subnet{SubnetId: f.id("subnet-bedrocksys"), CidrBlock: in.CidrBlock}}, nil
}

func (f *fakeSystemVPC) DeleteSubnet(context.Context, *ec2.DeleteSubnetInput, string) (*ec2.DeleteSubnetOutput, error) {
	return &ec2.DeleteSubnetOutput{}, nil
}

func (f *fakeSystemVPC) DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, string) (*ec2.DescribeSubnetsOutput, error) {
	return &ec2.DescribeSubnetsOutput{}, nil
}

func (f *fakeSystemVPC) CreateRouteTable(context.Context, *ec2.CreateRouteTableInput, string) (*ec2.CreateRouteTableOutput, error) {
	return &ec2.CreateRouteTableOutput{RouteTable: &ec2.RouteTable{RouteTableId: f.id("rtb")}}, nil
}

func (f *fakeSystemVPC) DeleteRouteTable(context.Context, *ec2.DeleteRouteTableInput, string) (*ec2.DeleteRouteTableOutput, error) {
	return &ec2.DeleteRouteTableOutput{}, nil
}

func (f *fakeSystemVPC) DescribeRouteTables(context.Context, *ec2.DescribeRouteTablesInput, string) (*ec2.DescribeRouteTablesOutput, error) {
	return &ec2.DescribeRouteTablesOutput{}, nil
}

func (f *fakeSystemVPC) CreateRoute(context.Context, *ec2.CreateRouteInput, string) (*ec2.CreateRouteOutput, error) {
	return &ec2.CreateRouteOutput{}, nil
}

func (f *fakeSystemVPC) AssociateRouteTable(context.Context, *ec2.AssociateRouteTableInput, string) (*ec2.AssociateRouteTableOutput, error) {
	return &ec2.AssociateRouteTableOutput{AssociationId: f.id("rtbassoc")}, nil
}

func (f *fakeSystemVPC) DisassociateRouteTable(context.Context, *ec2.DisassociateRouteTableInput, string) (*ec2.DisassociateRouteTableOutput, error) {
	return &ec2.DisassociateRouteTableOutput{}, nil
}

func (f *fakeSystemVPC) CreateNatGateway(context.Context, *ec2.CreateNatGatewayInput, string) (*ec2.CreateNatGatewayOutput, error) {
	return &ec2.CreateNatGatewayOutput{NatGateway: &ec2.NatGateway{NatGatewayId: f.id("nat")}}, nil
}

func (f *fakeSystemVPC) DeleteNatGateway(context.Context, *ec2.DeleteNatGatewayInput, string) (*ec2.DeleteNatGatewayOutput, error) {
	return &ec2.DeleteNatGatewayOutput{}, nil
}

func (f *fakeSystemVPC) DescribeNatGateways(context.Context, *ec2.DescribeNatGatewaysInput, string) (*ec2.DescribeNatGatewaysOutput, error) {
	return &ec2.DescribeNatGatewaysOutput{}, nil
}

func (f *fakeSystemVPC) AllocateAddress(context.Context, *ec2.AllocateAddressInput, string) (*ec2.AllocateAddressOutput, error) {
	return &ec2.AllocateAddressOutput{AllocationId: f.id("eipalloc"), PublicIp: aws.String("198.51.100.9")}, nil
}

func (f *fakeSystemVPC) ReleaseAddress(context.Context, *ec2.ReleaseAddressInput, string) (*ec2.ReleaseAddressOutput, error) {
	return &ec2.ReleaseAddressOutput{}, nil
}

func (f *fakeSystemVPC) CreateInternetGateway(context.Context, *ec2.CreateInternetGatewayInput, string) (*ec2.CreateInternetGatewayOutput, error) {
	f.igwCreated = true
	return &ec2.CreateInternetGatewayOutput{InternetGateway: &ec2.InternetGateway{InternetGatewayId: aws.String("igw-bedrocksys")}}, nil
}

func (f *fakeSystemVPC) AttachInternetGateway(context.Context, *ec2.AttachInternetGatewayInput, string) (*ec2.AttachInternetGatewayOutput, error) {
	return &ec2.AttachInternetGatewayOutput{}, nil
}

func (f *fakeSystemVPC) DetachInternetGateway(context.Context, *ec2.DetachInternetGatewayInput, string) (*ec2.DetachInternetGatewayOutput, error) {
	return &ec2.DetachInternetGatewayOutput{}, nil
}

func (f *fakeSystemVPC) DeleteInternetGateway(context.Context, *ec2.DeleteInternetGatewayInput, string) (*ec2.DeleteInternetGatewayOutput, error) {
	return &ec2.DeleteInternetGatewayOutput{}, nil
}

// DescribeInternetGateways answers the post-attach lookup: the builder
// attaches an IGW and then re-reads it, so the pre-create lookup must find
// nothing (or no IGW is ever created) and only the post-attach one answered.
func (f *fakeSystemVPC) DescribeInternetGateways(_ context.Context, in *ec2.DescribeInternetGatewaysInput, _ string) (*ec2.DescribeInternetGatewaysOutput, error) {
	if !f.igwCreated {
		return &ec2.DescribeInternetGatewaysOutput{}, nil
	}
	return &ec2.DescribeInternetGatewaysOutput{InternetGateways: []*ec2.InternetGateway{{
		InternetGatewayId: aws.String("igw-bedrocksys"),
		Attachments:       []*ec2.InternetGatewayAttachment{{VpcId: in.Filters[0].Values[0]}},
	}}}, nil
}

// testModelID is the real self-host catalog entry (MinVRAMMiB=5120), used
// rather than a fabricated one so tests exercise LookupServingSpec against
// the actual catalog data instead of a parallel test fixture.
const testModelID = "meta.llama3-2-1b-instruct-v1:0"

// fakeVPC is an in-memory launchVPCProvisioner: every CreateNetworkInterface
// call returns a fresh ENI ID/IP so concurrent launches never collide. Records
// are retained so DescribeNetworkInterfaces can answer the describe-or-create
// lookup the daemon port depends on for idempotence.
type fakeVPC struct {
	mu       sync.Mutex
	nextID   int
	created  []string
	deleted  []string
	detached []string
	records  map[string]*ec2.NetworkInterface
}

func (f *fakeVPC) CreateNetworkInterface(_ context.Context, in *ec2.CreateNetworkInterfaceInput, _ string) (*ec2.CreateNetworkInterfaceOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := f.nextID
	eniID := fmt.Sprintf("eni-%d", id)
	f.created = append(f.created, eniID)
	ni := &ec2.NetworkInterface{
		NetworkInterfaceId: aws.String(eniID),
		PrivateIpAddress:   aws.String(fmt.Sprintf("10.244.1.%d", id%250+1)),
		MacAddress:         aws.String("02:00:00:00:00:01"),
		SubnetId:           in.SubnetId,
		Description:        in.Description,
	}
	if f.records == nil {
		f.records = map[string]*ec2.NetworkInterface{}
	}
	f.records[eniID] = ni
	return &ec2.CreateNetworkInterfaceOutput{NetworkInterface: ni}, nil
}

// putRecord seeds an ENI the fake did not mint, so a test can present the
// lookup with a record it would not otherwise produce.
func (f *fakeVPC) putRecord(eniID, subnetID, description, ip, mac string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.records == nil {
		f.records = map[string]*ec2.NetworkInterface{}
	}
	f.records[eniID] = &ec2.NetworkInterface{
		NetworkInterfaceId: aws.String(eniID),
		SubnetId:           aws.String(subnetID),
		Description:        aws.String(description),
		PrivateIpAddress:   aws.String(ip),
		MacAddress:         aws.String(mac),
	}
}

// DescribeNetworkInterfaces honours only the subnet-id and description filters,
// which are the two the daemon-port lookup uses (ENIs carry no tag filter).
func (f *fakeVPC) DescribeNetworkInterfaces(_ context.Context, in *ec2.DescribeNetworkInterfacesInput, _ string) (*ec2.DescribeNetworkInterfacesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*ec2.NetworkInterface
	for _, ni := range f.records {
		match := true
		for _, filter := range in.Filters {
			var got string
			switch aws.StringValue(filter.Name) {
			case "subnet-id":
				got = aws.StringValue(ni.SubnetId)
			case "description":
				got = aws.StringValue(ni.Description)
			default:
				return nil, fmt.Errorf("fakeVPC: unsupported ENI filter %q", aws.StringValue(filter.Name))
			}
			if !slices.Contains(aws.StringValueSlice(filter.Values), got) {
				match = false
				break
			}
		}
		if match {
			out = append(out, ni)
		}
	}
	return &ec2.DescribeNetworkInterfacesOutput{NetworkInterfaces: out}, nil
}

func (f *fakeVPC) DeleteNetworkInterface(_ context.Context, in *ec2.DeleteNetworkInterfaceInput, _ string) (*ec2.DeleteNetworkInterfaceOutput, error) {
	f.mu.Lock()
	f.deleted = append(f.deleted, aws.StringValue(in.NetworkInterfaceId))
	delete(f.records, aws.StringValue(in.NetworkInterfaceId))
	f.mu.Unlock()
	return &ec2.DeleteNetworkInterfaceOutput{}, nil
}

func (f *fakeVPC) DetachENI(_ context.Context, _, eniID string) error {
	f.mu.Lock()
	f.detached = append(f.detached, eniID)
	f.mu.Unlock()
	return nil
}

// fakeVolume is an in-memory launchVolumeProvisioner.
type fakeVolume struct {
	mu      sync.Mutex
	nextID  int
	created []*ec2.CreateVolumeInput
	deleted []string
	// failCreate, when set, makes CreateVolume return this error instead.
	failCreate error
}

func (f *fakeVolume) CreateVolume(_ context.Context, in *ec2.CreateVolumeInput, _ string) (*ec2.Volume, error) {
	f.mu.Lock()
	f.created = append(f.created, in)
	f.mu.Unlock()
	if f.failCreate != nil {
		return nil, f.failCreate
	}
	f.mu.Lock()
	f.nextID++
	id := f.nextID
	f.mu.Unlock()
	return &ec2.Volume{VolumeId: aws.String(fmt.Sprintf("vol-%d", id))}, nil
}

func (f *fakeVolume) DeleteVolume(_ context.Context, in *ec2.DeleteVolumeInput, _ string) (*ec2.DeleteVolumeOutput, error) {
	f.mu.Lock()
	f.deleted = append(f.deleted, aws.StringValue(in.VolumeId))
	f.mu.Unlock()
	return &ec2.DeleteVolumeOutput{}, nil
}

// fakeAttacher is an in-memory volumeAttacher. failErr, when set, makes
// AttachVolume fail so launch_test.go can exercise the post-boot rollback path.
type fakeAttacher struct {
	failErr error
}

func (f *fakeAttacher) AttachVolume(_ context.Context, _, _, _, device string) (string, error) {
	if f.failErr != nil {
		return "", f.failErr
	}
	return device, nil
}

// fakeInstanceLauncher counts launches, so the concurrency test can assert
// exactly one VM was launched regardless of how many Ensure calls raced.
type fakeInstanceLauncher struct {
	mu          sync.Mutex
	launchCount atomic.Int32
	requests    []*sysinstance.SystemInstanceInput
	terminated  []string
	nextID      int
	failLaunch  error
}

var _ sysinstance.SystemInstanceLauncher = (*fakeInstanceLauncher)(nil)

func (f *fakeInstanceLauncher) LaunchSystemInstance(in *sysinstance.SystemInstanceInput) (*sysinstance.SystemInstanceOutput, error) {
	f.launchCount.Add(1)
	f.mu.Lock()
	f.requests = append(f.requests, in)
	f.mu.Unlock()
	if f.failLaunch != nil {
		return nil, f.failLaunch
	}
	f.mu.Lock()
	f.nextID++
	id := f.nextID
	f.mu.Unlock()
	return &sysinstance.SystemInstanceOutput{InstanceID: fmt.Sprintf("i-%d", id)}, nil
}

// lastInput returns the most recent LaunchSystemInstance request, for tests
// that assert on how a launch was wired.
func (f *fakeInstanceLauncher) lastInput() *sysinstance.SystemInstanceInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeInstanceLauncher) launches() []*sysinstance.SystemInstanceInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *fakeInstanceLauncher) TerminateSystemInstance(instanceID string) error {
	f.mu.Lock()
	f.terminated = append(f.terminated, instanceID)
	f.mu.Unlock()
	return nil
}

// fakeAMIResolver always resolves the same serving AMI.
type fakeAMIResolver struct{}

func (fakeAMIResolver) DescribeImages(_ context.Context, _ *ec2.DescribeImagesInput, _ string) (*ec2.DescribeImagesOutput, error) {
	return &ec2.DescribeImagesOutput{Images: []*ec2.Image{{
		ImageId:      aws.String("ami-vllm-serving"),
		CreationDate: aws.String("2026-01-01T00:00:00.000Z"),
	}}}, nil
}

// fakeWeightsResolver resolves a fixed snapshot ID for every model unless
// resolvable is false, mimicking "no weights staged".
type fakeWeightsResolver struct {
	snapshotID string
	resolvable bool
	err        error
}

func (f fakeWeightsResolver) Resolve(_ context.Context, _ string) (string, bool, error) {
	return f.snapshotID, f.resolvable, f.err
}

// stubRoundTripper returns statusCode for every request, letting readiness
// tests skip real networking entirely.
type stubRoundTripper struct {
	statusCode int32 // atomic: TestEnsure tests flip this to simulate boot delay
}

func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	code := int(atomic.LoadInt32(&s.statusCode))
	return &http.Response{
		StatusCode: code,
		Body:       http.NoBody,
		Header:     make(http.Header),
	}, nil
}

// fakeHostPort records the host-side ports a launch asked for, so tests can
// assert the daemon gets a routed presence before any VM is booted.
type fakeHostPort struct {
	mu      sync.Mutex
	ensured []hostPortCall
	failErr error
}

// hostPortCall captures the args of one EnsureVPCHostPort invocation.
type hostPortCall struct {
	eniID string
	mac   string
	addr  string
}

func (f *fakeHostPort) EnsureVPCHostPort(eniID, mac, addr string) error {
	f.mu.Lock()
	f.ensured = append(f.ensured, hostPortCall{eniID: eniID, mac: mac, addr: addr})
	f.mu.Unlock()
	return f.failErr
}

func (f *fakeHostPort) calls() []hostPortCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ensured)
}

// testNodeID names the daemon replica in tests, since a launch refuses to run
// without one (each node owns a distinct system-VPC port).
const testNodeID = "node-a"

// launchHarness bundles every LaunchDeps fake so a test can reach into any of
// them, mirroring handlers_rds's launchHarness.
type launchHarness struct {
	sysvpc   *fakeSystemVPC
	vpc      *fakeVPC
	launcher *fakeInstanceLauncher
	images   fakeAMIResolver
	volumes  *fakeVolume
	attacher *fakeAttacher
	weights  fakeWeightsResolver
	hostPort *fakeHostPort
}

func newLaunchHarness() *launchHarness {
	return &launchHarness{
		sysvpc:   &fakeSystemVPC{},
		vpc:      &fakeVPC{},
		launcher: &fakeInstanceLauncher{},
		volumes:  &fakeVolume{},
		attacher: &fakeAttacher{},
		weights:  fakeWeightsResolver{snapshotID: "snap-llama32-1b", resolvable: true},
		hostPort: &fakeHostPort{},
	}
}

func (h *launchHarness) deps() LaunchDeps {
	return LaunchDeps{
		Config:    &config.Config{Region: "us-east-1", AZ: "us-east-1a"},
		SystemVPC: h.sysvpc.deps(),
		VPC:       h.vpc,
		Instance:  h.launcher,
		Image:     h.images,
		Volume:    h.volumes,
		Attacher:  h.attacher,
		Weights:   h.weights,
		HostPort:  h.hostPort,
		NodeID:    testNodeID,
	}
}

func testLaunchInput() LaunchInput {
	return LaunchInput{
		ModelID:      testModelID,
		InstanceType: "g5.xlarge",
		VLLMArgs:     []string{"--dtype=bfloat16"},
	}
}
