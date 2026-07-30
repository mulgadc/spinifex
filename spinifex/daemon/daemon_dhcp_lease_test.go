package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_eip "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eip"
	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubEIPService stands in for the EIP service without implementing either
// optional interface, so it exercises the "external IPAM disabled" paths.
type stubEIPService struct {
	handlers_ec2_eip.EIPService
}

// stubEIPChecker answers allocation lookups only.
type stubEIPChecker struct {
	handlers_ec2_eip.EIPService

	exists bool
	err    error
}

func (s *stubEIPChecker) AllocationExists(context.Context, string) (bool, error) {
	return s.exists, s.err
}

// stubEIPRebinder records the address move it was asked to make.
type stubEIPRebinder struct {
	handlers_ec2_eip.EIPService

	oldIP, newIP string
	err          error
}

func (s *stubEIPRebinder) RebindPublicIP(_ context.Context, _, oldIP, newIP string) error {
	s.oldIP, s.newIP = oldIP, newIP
	return s.err
}

func TestLeaseOwnerStatusEIP(t *testing.T) {
	tests := []struct {
		name    string
		checker *stubEIPChecker
		want    string
		wantErr bool
	}{
		{name: "allocation survives", checker: &stubEIPChecker{exists: true}, want: dhcp.OwnerStatusAlive},
		{name: "allocation deleted", checker: &stubEIPChecker{}, want: dhcp.OwnerStatusGone},
		{
			name:    "lookup failed is never gone",
			checker: &stubEIPChecker{err: errors.New("kv timeout")},
			want:    dhcp.OwnerStatusUnknown,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Daemon{eipService: tt.checker}
			status, err := d.leaseOwnerStatus(t.Context(), dhcp.OwnerCheckRequest{
				ClientID: "eipalloc-1", Purpose: dhcp.PurposeEIP,
			})
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.want, status)
		})
	}
}

// A stub EIP service (no external IPAM) can prove nothing about an allocation,
// so it must not be read as a deletion.
func TestLeaseOwnerStatusEIPWithoutIPAMIsUnknown(t *testing.T) {
	d := &Daemon{eipService: &stubEIPService{}}
	status, err := d.leaseOwnerStatus(t.Context(), dhcp.OwnerCheckRequest{
		ClientID: "eipalloc-1", Purpose: dhcp.PurposeEIP,
	})
	require.Error(t, err)
	assert.Equal(t, dhcp.OwnerStatusUnknown, status)
}

func TestLeaseOwnerStatusWithoutVPCServiceIsUnknown(t *testing.T) {
	d := &Daemon{}
	for _, req := range []dhcp.OwnerCheckRequest{
		{ClientID: "eni-1", Purpose: dhcp.PurposeENIPublic},
		{ClientID: "dhcp-gw-lrp-vpc-1", Purpose: dhcp.PurposeGatewayLRP, VPCID: "vpc-1"},
	} {
		status, err := d.leaseOwnerStatus(t.Context(), req)
		require.Error(t, err)
		assert.Equal(t, dhcp.OwnerStatusUnknown, status)
	}
}

// An unrecognised purpose may well have a live owner this daemon cannot see.
func TestLeaseOwnerStatusUnknownPurposeIsUnknown(t *testing.T) {
	d := &Daemon{}
	status, err := d.leaseOwnerStatus(t.Context(), dhcp.OwnerCheckRequest{
		ClientID: "x-1", Purpose: "natgw-external",
	})
	require.Error(t, err)
	assert.Equal(t, dhcp.OwnerStatusUnknown, status)
}

func TestLeaseOwnerStatusVPCRecords(t *testing.T) {
	d := createVPCTestDaemon(t)

	out, err := d.vpcService.CreateVpc(t.Context(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("172.31.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)
	liveVPC := *out.Vpc.VpcId

	status, err := d.leaseOwnerStatus(t.Context(), dhcp.OwnerCheckRequest{
		ClientID: dhcp.GatewayLRPClientID(liveVPC), Purpose: dhcp.PurposeGatewayLRP, VPCID: liveVPC,
	})
	require.NoError(t, err)
	assert.Equal(t, dhcp.OwnerStatusAlive, status)

	// A lease for a VPC that no longer has a record is the leak the reaper exists
	// to clear, so it must read as gone rather than unknown.
	status, err = d.leaseOwnerStatus(t.Context(), dhcp.OwnerCheckRequest{
		ClientID: "dhcp-gw-lrp-vpc-deleted", Purpose: dhcp.PurposeGatewayLRP, VPCID: "vpc-deleted",
	})
	require.NoError(t, err)
	assert.Equal(t, dhcp.OwnerStatusGone, status)

	status, err = d.leaseOwnerStatus(t.Context(), dhcp.OwnerCheckRequest{
		ClientID: "eni-deleted", Purpose: dhcp.PurposeENIPublic,
	})
	require.NoError(t, err)
	assert.Equal(t, dhcp.OwnerStatusGone, status)
}

// Without a vpc_id there is nothing to look up, and guessing would risk
// releasing a live gateway's address.
func TestLeaseOwnerStatusGatewayWithoutVPCIDIsUnknown(t *testing.T) {
	d := createVPCTestDaemon(t)

	status, err := d.leaseOwnerStatus(t.Context(), dhcp.OwnerCheckRequest{
		ClientID: "dhcp-gw-lrp-vpc-1", Purpose: dhcp.PurposeGatewayLRP,
	})
	require.Error(t, err)
	assert.Equal(t, dhcp.OwnerStatusUnknown, status)
}

// ownerCheckRoundTrip drives the handler over NATS, so the reply encoding is
// exercised the way the reaper sees it.
func ownerCheckRoundTrip(t *testing.T, d *Daemon, payload []byte) dhcp.OwnerCheckReply {
	t.Helper()
	subject := "test.dhcp.owner-check." + t.Name()
	sub, err := d.natsConn.Subscribe(subject, d.handleDHCPOwnerCheck)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	msg, err := d.natsConn.Request(subject, payload, 5*time.Second)
	require.NoError(t, err)

	var reply dhcp.OwnerCheckReply
	require.NoError(t, json.Unmarshal(msg.Data, &reply))
	return reply
}

func TestHandleDHCPOwnerCheckAnswersRequests(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)
	d.eipService = &stubEIPChecker{exists: true}

	req, err := json.Marshal(dhcp.OwnerCheckRequest{ClientID: "eipalloc-1", Purpose: dhcp.PurposeEIP})
	require.NoError(t, err)

	reply := ownerCheckRoundTrip(t, d, req)
	assert.Equal(t, dhcp.OwnerStatusAlive, reply.Status)
	assert.Empty(t, reply.Error)
}

func TestHandleDHCPOwnerCheckRejectsGarbage(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)

	reply := ownerCheckRoundTrip(t, d, []byte("not json"))
	assert.Equal(t, dhcp.OwnerStatusUnknown, reply.Status)
	assert.Contains(t, reply.Error, "decode owner-check request")
}

// A published (rather than requested) check has nowhere to reply; it must be
// logged instead of panicking on an empty subject.
func TestRespondOwnerCheckWithoutReplySubject(t *testing.T) {
	assert.NotPanics(t, func() {
		respondOwnerCheck(&nats.Msg{}, dhcp.OwnerStatusGone, nil)
		respondOwnerCheck(&nats.Msg{}, dhcp.OwnerStatusUnknown, errors.New("kv timeout"))
	})
}

func TestRebindLeaseRecordEIP(t *testing.T) {
	rebinder := &stubEIPRebinder{}
	d := &Daemon{eipService: rebinder}

	require.NoError(t, d.rebindLeaseRecord(t.Context(), dhcp.LeaseChangedRequest{
		ClientID: "eipalloc-1", Purpose: dhcp.PurposeEIP,
		OldIP: "192.168.1.139", NewIP: "192.168.1.134",
	}))
	assert.Equal(t, "192.168.1.139", rebinder.oldIP)
	assert.Equal(t, "192.168.1.134", rebinder.newIP)
}

func TestRebindLeaseRecordErrors(t *testing.T) {
	tests := []struct {
		name string
		d    *Daemon
		req  dhcp.LeaseChangedRequest
	}{
		{
			name: "EIP service cannot rebind",
			d:    &Daemon{eipService: &stubEIPService{}},
			req:  dhcp.LeaseChangedRequest{ClientID: "eipalloc-1", Purpose: dhcp.PurposeEIP},
		},
		{
			name: "no VPC service for ENI",
			d:    &Daemon{},
			req:  dhcp.LeaseChangedRequest{ClientID: "eni-1", Purpose: dhcp.PurposeENIPublic},
		},
		{
			name: "no owner for purpose",
			d:    &Daemon{},
			req: dhcp.LeaseChangedRequest{
				ClientID: "gw-1", Purpose: dhcp.PurposeGatewayLRP,
				OldIP: "192.168.1.1", NewIP: "192.168.1.2",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Error(t, tt.d.rebindLeaseRecord(t.Context(), tt.req))
		})
	}
}

// leaseChangedRoundTrip drives the handler over NATS, matching how vpcd calls it.
func leaseChangedRoundTrip(t *testing.T, d *Daemon, payload []byte) dhcp.LeaseChangedReply {
	t.Helper()
	subject := "test.dhcp.lease-changed." + t.Name()
	sub, err := d.natsConn.Subscribe(subject, d.handleDHCPLeaseChanged)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	msg, err := d.natsConn.Request(subject, payload, 5*time.Second)
	require.NoError(t, err)

	var reply dhcp.LeaseChangedReply
	require.NoError(t, json.Unmarshal(msg.Data, &reply))
	return reply
}

func TestHandleDHCPLeaseChangedRebinds(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)
	rebinder := &stubEIPRebinder{}
	d.eipService = rebinder

	req, err := json.Marshal(dhcp.LeaseChangedRequest{
		ClientID: "eipalloc-1", Purpose: dhcp.PurposeEIP,
		OldIP: "192.168.1.139", NewIP: "192.168.1.134",
	})
	require.NoError(t, err)

	reply := leaseChangedRoundTrip(t, d, req)
	assert.Empty(t, reply.Error)
	assert.Equal(t, "192.168.1.134", rebinder.newIP)
}

// vpcd needs the failure back on the reply: it has already released the old
// address, so a silent error leaves a record naming an address nobody holds.
func TestHandleDHCPLeaseChangedReportsFailures(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)
	d.eipService = &stubEIPRebinder{err: errors.New("record vanished")}

	req, err := json.Marshal(dhcp.LeaseChangedRequest{
		ClientID: "eipalloc-1", Purpose: dhcp.PurposeEIP,
		OldIP: "192.168.1.139", NewIP: "192.168.1.134",
	})
	require.NoError(t, err)

	assert.Contains(t, leaseChangedRoundTrip(t, d, req).Error, "record vanished")
}

func TestHandleDHCPLeaseChangedRejectsIncompleteRequests(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)

	assert.Contains(t, leaseChangedRoundTrip(t, d, []byte("not json")).Error, "decode lease-changed request")

	req, err := json.Marshal(dhcp.LeaseChangedRequest{Purpose: dhcp.PurposeEIP, NewIP: "192.168.1.134"})
	require.NoError(t, err)
	assert.Contains(t, leaseChangedRoundTrip(t, d, req).Error, "needs client_id and new_ip")
}

func TestRespondLeaseChangedWithoutReplySubject(t *testing.T) {
	assert.NotPanics(t, func() {
		respondLeaseChanged(&nats.Msg{}, nil)
		respondLeaseChanged(&nats.Msg{}, errors.New("rebind failed"))
	})
}
