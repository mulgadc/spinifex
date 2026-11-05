package accountlifecycle

//test:in-package — the reapers are deliberately unexported so no ordinary
// request path can reach a force delete, and their listing filters and delete
// ordering are the whole substance of the package.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCluster answers the EC2 subjects the reapers drive, recording the order
// they arrive in. Detach-before-delete and disassociate-before-release are
// ordering rules, so a double that only recorded a set would not test them.
type fakeCluster struct {
	nc *nats.Conn

	mu        sync.Mutex
	calls     []string
	requests  map[string][]byte
	replies   map[string]any
	errorCode map[string]string
	handlers  map[string]func(data []byte) (reply any, errorCode string)
}

func newFakeCluster(t *testing.T) *fakeCluster {
	t.Helper()
	_, nc := testutil.StartTestNATS(t)

	cluster := &fakeCluster{
		nc:        nc,
		requests:  map[string][]byte{},
		replies:   map[string]any{},
		errorCode: map[string]string{},
		handlers:  map[string]func([]byte) (any, string){},
	}

	sub, err := nc.Subscribe("ec2.>", cluster.serve)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())

	return cluster
}

// fakeClusterNode is the one node the fake cluster answers as. Replies carry
// its ID because an inventory fan-out only counts attributable frames.
const fakeClusterNode = "node-1"

func (c *fakeCluster) serve(msg *nats.Msg) {
	c.mu.Lock()
	c.calls = append(c.calls, msg.Subject)
	c.requests[msg.Subject] = msg.Data
	code, isError := c.errorCode[msg.Subject]
	reply, hasReply := c.replies[msg.Subject]
	handler, hasHandler := c.handlers[msg.Subject]
	c.mu.Unlock()

	respond := func(data []byte) {
		out := nats.NewMsg(msg.Reply)
		out.Data = data
		out.Header.Set(natsmsg.NodeIDHeader, fakeClusterNode)
		_ = msg.RespondMsg(out)
	}
	if hasHandler && !isError {
		reply, code = handler(msg.Data)
		isError, hasReply = code != "", true
	}

	switch {
	case isError:
		respond(awserrors.GenerateErrorPayload(code))
	case hasReply:
		payload, err := json.Marshal(reply)
		if err != nil {
			respond(awserrors.GenerateErrorPayload("InternalError"))
			return
		}
		respond(payload)
	default:
		respond([]byte(`{}`))
	}
}

func (c *fakeCluster) reply(subject string, output any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.replies[subject] = output
}

func (c *fakeCluster) fail(subject, code string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errorCode[subject] = code
}

// handle answers subject from state the test owns, for doubles whose answer
// must change as the reaper acts — an attachment a force detach clears.
func (c *fakeCluster) handle(subject string, handler func(data []byte) (any, string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers[subject] = handler
}

// echoAsNode adds a second reply to every request on subject, attributed to
// node, so one node can answer twice while another stays silent.
func (c *fakeCluster) echoAsNode(t *testing.T, subject, node string) {
	t.Helper()
	sub, err := c.nc.Subscribe(subject, func(msg *nats.Msg) {
		out := nats.NewMsg(msg.Reply)
		out.Data = []byte(`{}`)
		out.Header.Set(natsmsg.NodeIDHeader, node)
		_ = msg.RespondMsg(out)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, c.nc.Flush())
}

func (c *fakeCluster) called() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *fakeCluster) request(t *testing.T, subject string, into any) {
	t.Helper()
	c.mu.Lock()
	payload, ok := c.requests[subject]
	c.mu.Unlock()
	require.True(t, ok, "subject %s was never called", subject)
	require.NoError(t, json.Unmarshal(payload, into))
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// silentClusterCtx outlasts the full fan-out deadline a silent node costs.
func silentClusterCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// The stage graph is what keeps teardown converging, so it is asserted rather
// than left to the order the constructor happens to list.
func TestEC2ReapersAreOrderedByDependency(t *testing.T) {
	reapers := EC2Reapers(nil, 1, []string{"node-1"})

	var kinds []string
	for _, reaper := range reapers {
		kinds = append(kinds, reaper.Kind())
	}

	assert.Equal(t, []string{
		"snapshot", "image",
		"address", "network-interface", "internet-gateway", "route-table", "subnet", "security-group", "vpc",
	}, kinds)

	assert.Equal(t, StageNetwork, reapers[indexOfKind(reapers, "vpc")].Stage())
}

func indexOfKind(reapers []Reaper, kind string) int {
	for i, reaper := range reapers {
		if reaper.Kind() == kind {
			return i
		}
	}
	return -1
}

// heldVolume is one volume record as its owner keeps it: the ordinary delete
// refuses while the record names an instance, and a force detach clears it.
type heldVolume struct {
	mu       sync.Mutex
	id       string
	holder   string
	deleted  bool
	detaches int
}

func (v *heldVolume) state() (deleted bool, holder string, detaches int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.deleted, v.holder, v.detaches
}

func instancesIn(state string, instanceIDs ...string) ec2.DescribeInstancesOutput {
	reservation := &ec2.Reservation{ReservationId: aws.String("r-" + state)}
	for _, id := range instanceIDs {
		reservation.Instances = append(reservation.Instances, &ec2.Instance{
			InstanceId: aws.String(id), State: &ec2.InstanceState{Name: aws.String(state)},
		})
	}
	return ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{reservation}}
}

// Owner-scoped: an image the account can merely see is not its property and
// must survive its deletion.
func TestImageReaperListsOnlyOwnedImages(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.reply("ec2.DescribeImages", ec2.DescribeImagesOutput{
		Images: []*ec2.Image{{ImageId: aws.String("ami-owned")}, {}},
	})

	reaper := &imageReaper{nc: cluster.nc}
	found, err := reaper.List(testCtx(t), "000000000042")
	require.NoError(t, err)

	require.Len(t, found, 1)
	assert.Equal(t, "ami-owned", found[0].ID)

	var input ec2.DescribeImagesInput
	cluster.request(t, "ec2.DescribeImages", &input)
	require.Len(t, input.Owners, 1)
	assert.Equal(t, "000000000042", aws.StringValue(input.Owners[0]))
}

func TestSnapshotReaperListsOnlyOwnedSnapshots(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.reply("ec2.DescribeSnapshots", ec2.DescribeSnapshotsOutput{
		Snapshots: []*ec2.Snapshot{{SnapshotId: aws.String("snap-1")}, {}},
	})

	reaper := &snapshotReaper{nc: cluster.nc}
	found, err := reaper.List(testCtx(t), "000000000042")
	require.NoError(t, err)

	require.Len(t, found, 1)
	assert.Equal(t, "snap-1", found[0].ID)

	var input ec2.DescribeSnapshotsInput
	cluster.request(t, "ec2.DescribeSnapshots", &input)
	require.Len(t, input.OwnerIds, 1)
	assert.Equal(t, "000000000042", aws.StringValue(input.OwnerIds[0]))
}

// An associated address cannot be released, and the association routinely
// outlives the instance by a moment.
func TestAddressReaperDisassociatesBeforeReleasing(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.reply("ec2.DescribeAddresses", ec2.DescribeAddressesOutput{
		Addresses: []*ec2.Address{
			{
				AllocationId:  aws.String("eipalloc-1"),
				AssociationId: aws.String("eipassoc-1"),
				PublicIp:      aws.String("203.0.113.7"),
			},
			{PublicIp: aws.String("203.0.113.8")},
		},
	})

	reaper := &addressReaper{nc: cluster.nc}
	found, err := reaper.List(testCtx(t), "000000000042")
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "203.0.113.7", found[0].Detail)

	require.NoError(t, reaper.Delete(testCtx(t), "000000000042", found[0], false))
	assert.Equal(t, []string{
		"ec2.DescribeAddresses", "ec2.DescribeAddresses",
		"ec2.DisassociateAddress", "ec2.ReleaseAddress",
	}, cluster.called())
}

// The allocation id is not the association id. Sending the former is answered
// InvalidAssociationID.NotFound, which reads as never-associated, so the
// address stays associated and the release is refused.
func TestAddressReaperDisassociatesByAssociationID(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.reply("ec2.DescribeAddresses", ec2.DescribeAddressesOutput{
		Addresses: []*ec2.Address{{
			AllocationId:  aws.String("eipalloc-1"),
			AssociationId: aws.String("eipassoc-1"),
			PublicIp:      aws.String("203.0.113.7"),
		}},
	})

	reaper := &addressReaper{nc: cluster.nc}
	require.NoError(t, reaper.Delete(testCtx(t), "000000000042",
		Resource{Kind: "address", ID: "eipalloc-1", Detail: "203.0.113.7"}, false))

	var disassociate ec2.DisassociateAddressInput
	cluster.request(t, "ec2.DisassociateAddress", &disassociate)
	assert.Equal(t, "eipassoc-1", aws.StringValue(disassociate.AssociationId))

	var describe ec2.DescribeAddressesInput
	cluster.request(t, "ec2.DescribeAddresses", &describe)
	require.Len(t, describe.AllocationIds, 1)
	assert.Equal(t, "eipalloc-1", aws.StringValue(describe.AllocationIds[0]))
}

// An address that was never associated is the normal case, not a failure. It
// has nothing to disassociate, so the call is skipped rather than made and
// forgiven — a real error from it should still stop the release.
func TestAddressReaperReleasesAnUnassociatedAddress(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.reply("ec2.DescribeAddresses", ec2.DescribeAddressesOutput{
		Addresses: []*ec2.Address{{AllocationId: aws.String("eipalloc-1"), PublicIp: aws.String("203.0.113.7")}},
	})

	reaper := &addressReaper{nc: cluster.nc}
	err := reaper.Delete(testCtx(t), "000000000042", Resource{Kind: "address", ID: "eipalloc-1"}, false)

	require.NoError(t, err)
	assert.Equal(t, []string{"ec2.DescribeAddresses", "ec2.ReleaseAddress"}, cluster.called())
}

// An address already gone by the time its stage runs is a success: teardown
// re-runs after a crash and would otherwise never finish.
func TestAddressReaperTreatsAMissingAddressAsDeleted(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.fail("ec2.DescribeAddresses", "InvalidAllocationID.NotFound")
	cluster.fail("ec2.ReleaseAddress", "InvalidAllocationID.NotFound")

	reaper := &addressReaper{nc: cluster.nc}
	err := reaper.Delete(testCtx(t), "000000000042", Resource{Kind: "address", ID: "eipalloc-1"}, false)

	require.NoError(t, err)
	assert.Equal(t, []string{"ec2.DescribeAddresses", "ec2.ReleaseAddress"}, cluster.called())
}

// An attached gateway cannot be deleted, and its VPC cannot be deleted while
// it is attached — so the attachment is carried from the listing to the delete.
func TestIGWReaperDetachesBeforeDeleting(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.reply("ec2.DescribeInternetGateways", ec2.DescribeInternetGatewaysOutput{
		InternetGateways: []*ec2.InternetGateway{{
			InternetGatewayId: aws.String("igw-1"),
			Attachments:       []*ec2.InternetGatewayAttachment{{VpcId: aws.String("vpc-1")}},
		}},
	})

	reaper := &igwReaper{nc: cluster.nc}
	found, err := reaper.List(testCtx(t), "000000000042")
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "vpc-1", found[0].Detail)

	require.NoError(t, reaper.Delete(testCtx(t), "000000000042", found[0], false))
	assert.Equal(t, []string{
		"ec2.DescribeInternetGateways", "ec2.DetachInternetGateway", "ec2.DeleteInternetGateway",
	}, cluster.called())
}

// A detached gateway needs no detach call, and issuing one would fail.
func TestIGWReaperDeletesADetachedGatewayDirectly(t *testing.T) {
	cluster := newFakeCluster(t)

	reaper := &igwReaper{nc: cluster.nc}
	require.NoError(t, reaper.Delete(testCtx(t), "000000000042",
		Resource{Kind: "internet-gateway", ID: "igw-1"}, false))

	assert.Equal(t, []string{"ec2.DeleteInternetGateway"}, cluster.called())
}

// A gateway whose attach is not yet confirmed reports no attachment, so the
// listing carries no VPC and the delete is refused. That is not a wedge: the
// VPC survives because DeleteVpc rejects while the gateway is attached, and the
// stage re-sweeps until the confirmation lands.
func TestIGWReaperReportsARefusedDeleteForAnUnconfirmedAttach(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.fail("ec2.DeleteInternetGateway", awserrors.ErrorDependencyViolation)

	reaper := &igwReaper{nc: cluster.nc}
	err := reaper.Delete(testCtx(t), "000000000042",
		Resource{Kind: "internet-gateway", ID: "igw-1"}, false)

	require.Error(t, err)
	assert.Equal(t, []string{"ec2.DeleteInternetGateway"}, cluster.called(),
		"the reaper must not guess at the VPC: the next sweep sees the confirmed attachment")
}

// A VPC's main route table is deleted with the VPC and cannot be deleted on
// its own, so listing it would guarantee a stuck report every time.
func TestRouteTableReaperSkipsTheMainTable(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.reply("ec2.DescribeRouteTables", ec2.DescribeRouteTablesOutput{
		RouteTables: []*ec2.RouteTable{
			{
				RouteTableId: aws.String("rtb-main"),
				Associations: []*ec2.RouteTableAssociation{{Main: aws.Bool(true)}},
			},
			{
				RouteTableId: aws.String("rtb-custom"),
				Associations: []*ec2.RouteTableAssociation{
					{RouteTableAssociationId: aws.String("rtbassoc-1")},
					{RouteTableAssociationId: aws.String("rtbassoc-2")},
					nil,
				},
			},
		},
	})

	reaper := &routeTableReaper{nc: cluster.nc}
	found, err := reaper.List(testCtx(t), "000000000042")
	require.NoError(t, err)

	require.Len(t, found, 1)
	assert.Equal(t, "rtb-custom", found[0].ID)
	assert.Equal(t, "rtbassoc-1,rtbassoc-2", found[0].Detail)

	require.NoError(t, reaper.Delete(testCtx(t), "000000000042", found[0], false))
	assert.Equal(t, []string{
		"ec2.DescribeRouteTables",
		"ec2.DisassociateRouteTable", "ec2.DisassociateRouteTable",
		"ec2.DeleteRouteTable",
	}, cluster.called())
}

// The default group is created and destroyed with its VPC and cannot be
// deleted on its own.
func TestSecurityGroupReaperSkipsTheDefaultGroup(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.reply("ec2.DescribeSecurityGroups", ec2.DescribeSecurityGroupsOutput{
		SecurityGroups: []*ec2.SecurityGroup{
			{GroupId: aws.String("sg-default"), GroupName: aws.String("default")},
			{GroupId: aws.String("sg-app"), GroupName: aws.String("app")},
			{GroupName: aws.String("no-id")},
		},
	})

	reaper := &securityGroupReaper{nc: cluster.nc}
	found, err := reaper.List(testCtx(t), "000000000042")
	require.NoError(t, err)

	require.Len(t, found, 1)
	assert.Equal(t, "sg-app", found[0].ID)
}

func TestSubnetAndVPCReapersReportTheirVPC(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.reply("ec2.DescribeSubnets", ec2.DescribeSubnetsOutput{
		Subnets: []*ec2.Subnet{{SubnetId: aws.String("subnet-1"), VpcId: aws.String("vpc-1")}, {}},
	})
	cluster.reply("ec2.DescribeVpcs", ec2.DescribeVpcsOutput{
		Vpcs: []*ec2.Vpc{
			{VpcId: aws.String("vpc-1"), IsDefault: aws.Bool(true)},
			{VpcId: aws.String("vpc-2")},
			{},
		},
	})

	subnets, err := (&subnetReaper{nc: cluster.nc}).List(testCtx(t), "000000000042")
	require.NoError(t, err)
	require.Len(t, subnets, 1)
	assert.Equal(t, "vpc-1", subnets[0].Detail)

	vpcs, err := (&vpcReaper{nc: cluster.nc}).List(testCtx(t), "000000000042")
	require.NoError(t, err)
	require.Len(t, vpcs, 2)
	// The default VPC is deleted with the account like any other: an account
	// being torn down has no use for one.
	assert.Equal(t, "default", vpcs[0].Detail)
	assert.Empty(t, vpcs[1].Detail)
}

// A listing that fails must abort the teardown. Reading a failed listing as an
// empty account is how a stage reports itself drained while resources remain.
func TestAListingFailureIsReported(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.fail("ec2.DescribeSnapshots", "InternalError")

	_, err := (&snapshotReaper{nc: cluster.nc}).List(testCtx(t), "000000000042")

	assert.Error(t, err)
}

// Teardown re-runs after a crash, so an already-missing resource is a success.
// The customer API fails closed on these, which is right for a customer and
// wrong for a reaper.
func TestAlreadyGoneRecognisesMissingResources(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is gone", nil, true},
		{"ec2 not found", assertError("InvalidVolume.NotFound"), true},
		{"iam not found", assertError("NoSuchEntity: no such user"), true},
		{"allocation not found", assertError("InvalidAllocationID.NotFound"), true},
		{"still in use", assertError("VolumeInUse"), false},
		{"internal error", assertError("InternalError"), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isAlreadyGone(tc.err))
			if tc.want {
				assert.NoError(t, ignoreAlreadyGone(tc.err))
			} else {
				assert.Error(t, ignoreAlreadyGone(tc.err))
			}
		})
	}
}

func TestNotAssociatedIsBenign(t *testing.T) {
	assert.True(t, isNotAssociated(assertError("InvalidAssociationID.NotFound")))
	assert.False(t, isNotAssociated(assertError("AuthFailure")))
	assert.False(t, isNotAssociated(nil))
}

type stringError string

func (e stringError) Error() string { return string(e) }

func assertError(message string) error { return stringError(message) }

// An interface still attached cannot be deleted, and an interface that will not
// delete holds its subnet, which holds the VPC, which leaves the account in
// TERMINATING for good.
//
// The detach is dispatched to the owning instance as a command, not as its own
// EC2 call, so ec2.cmd.<instance> is what proves it was driven.
func TestNetworkInterfaceReaperDetachesBeforeDeleting(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.reply("ec2.DescribeNetworkInterfaces", ec2.DescribeNetworkInterfacesOutput{
		NetworkInterfaces: []*ec2.NetworkInterface{{
			NetworkInterfaceId: aws.String("eni-1"),
			SubnetId:           aws.String("subnet-1"),
			Attachment: &ec2.NetworkInterfaceAttachment{
				AttachmentId: aws.String("eni-attach-1"),
				InstanceId:   aws.String("i-1"),
			},
		}},
	})

	reaper := &networkInterfaceReaper{nc: cluster.nc}
	found, err := reaper.List(testCtx(t), "000000000042")
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "subnet-1", found[0].Detail)

	require.NoError(t, reaper.Delete(testCtx(t), "000000000042", found[0], false))
	assert.Contains(t, cluster.called(), "ec2.cmd.i-1", "the detach was never dispatched")
	assert.Contains(t, cluster.called(), "ec2.DeleteNetworkInterface")
}

// An interface whose instance is already gone is the wedged case: the detach
// cannot resolve an owner and answers NotFound, which must not stop the delete
// — the interface is exactly what has to go.
func TestNetworkInterfaceReaperDeletesWhenTheOwnerIsGone(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.reply("ec2.DescribeNetworkInterfaces", ec2.DescribeNetworkInterfacesOutput{
		NetworkInterfaces: []*ec2.NetworkInterface{{
			NetworkInterfaceId: aws.String("eni-1"),
			SubnetId:           aws.String("subnet-1"),
			Attachment:         &ec2.NetworkInterfaceAttachment{AttachmentId: aws.String("eni-attach-1")},
		}},
	})

	reaper := &networkInterfaceReaper{nc: cluster.nc}
	err := reaper.Delete(testCtx(t), "000000000042",
		Resource{Kind: "network-interface", ID: "eni-1"}, false)

	require.NoError(t, err)
	assert.Contains(t, cluster.called(), "ec2.DeleteNetworkInterface")
}

// The interface left behind by a half-finished launch is the case this reaper
// exists for, and it is unattached: there is nothing to detach, so the detach
// is skipped rather than made and forgiven.
func TestNetworkInterfaceReaperDeletesAnUnattachedInterface(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.reply("ec2.DescribeNetworkInterfaces", ec2.DescribeNetworkInterfacesOutput{
		NetworkInterfaces: []*ec2.NetworkInterface{{
			NetworkInterfaceId: aws.String("eni-1"), SubnetId: aws.String("subnet-1"),
		}},
	})

	reaper := &networkInterfaceReaper{nc: cluster.nc}
	err := reaper.Delete(testCtx(t), "000000000042", Resource{Kind: "network-interface", ID: "eni-1"}, false)

	require.NoError(t, err)
	assert.Equal(t, []string{
		"ec2.DescribeNetworkInterfaces", "ec2.DeleteNetworkInterface",
	}, cluster.called())
}

// An interface already gone by the time its stage runs is a success: teardown
// re-runs after a crash and would otherwise never finish.
func TestNetworkInterfaceReaperTreatsAMissingInterfaceAsDeleted(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.fail("ec2.DescribeNetworkInterfaces", "InvalidNetworkInterfaceID.NotFound")
	cluster.fail("ec2.DeleteNetworkInterface", "InvalidNetworkInterfaceID.NotFound")

	reaper := &networkInterfaceReaper{nc: cluster.nc}
	err := reaper.Delete(testCtx(t), "000000000042", Resource{Kind: "network-interface", ID: "eni-1"}, false)

	require.NoError(t, err)
	assert.Equal(t, []string{
		"ec2.DescribeNetworkInterfaces", "ec2.DeleteNetworkInterface",
	}, cluster.called())
}

// Interfaces are reaped before subnets. The reverse order cannot work: the
// subnet delete is refused while an interface is still in it.
func TestEC2ReapersRemoveInterfacesBeforeSubnets(t *testing.T) {
	var eni, subnet = -1, -1
	for i, reaper := range EC2Reapers(nil, 1, []string{"node-1"}) {
		switch reaper.Kind() {
		case "network-interface":
			eni = i
		case "subnet":
			subnet = i
		}
	}
	require.NotEqual(t, -1, eni, "there is no network-interface reaper")
	require.Less(t, eni, subnet, "interfaces must be reaped before the subnets holding them")
}

