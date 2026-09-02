package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/admin"
	handlers_ec2_igw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/igw"
	handlers_ec2_routetable "github.com/mulgadc/spinifex/spinifex/handlers/ec2/routetable"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createDefaultVPCTestDaemon wires the VPC, IGW and route table services onto a
// single JetStream, and returns the connection so tests can drive the handlers
// through real request/reply rather than a synthetic message.
func createDefaultVPCTestDaemon(t *testing.T) (*Daemon, *nats.Conn) {
	t.Helper()

	daemon := createTestDaemon(t, sharedNATSURL)

	_, nc, _ := testutil.StartTestJetStream(t)
	testutil.StubVpcdSGResponder(t, nc)

	vpcSvc, err := handlers_ec2_vpc.NewVPCServiceImplWithNATS(t.Context(), daemon.config, nc)
	require.NoError(t, err)
	daemon.vpcService = vpcSvc

	igwSvc, err := handlers_ec2_igw.NewIGWServiceImplWithNATS(t.Context(), daemon.config, nc)
	require.NoError(t, err)
	daemon.igwService = igwSvc

	rtbSvc, err := handlers_ec2_routetable.NewRouteTableServiceImplWithNATS(t.Context(), daemon.config, nc)
	require.NoError(t, err)
	daemon.routeTableService = rtbSvc

	return daemon, nc
}

// defaultVPCID returns the account's default VPC ID, or "" when it has none.
func defaultVPCID(t *testing.T, d *Daemon, accountID string) string {
	t.Helper()
	out, err := d.vpcService.DescribeVpcs(t.Context(), &ec2.DescribeVpcsInput{}, accountID)
	require.NoError(t, err)
	for _, vpc := range out.Vpcs {
		if aws.BoolValue(vpc.IsDefault) {
			return aws.StringValue(vpc.VpcId)
		}
	}
	return ""
}

func TestHandleAccountCreated_BuildsDefaultVPCInfrastructure(t *testing.T) {
	daemon, _ := createDefaultVPCTestDaemon(t)

	const accountID = "000000000077"
	evt, err := json.Marshal(map[string]string{"account_id": accountID})
	require.NoError(t, err)

	outcome := daemon.handleAccountCreated(&nats.Msg{Subject: "account.created", Data: evt})
	assert.Equal(t, outcomeSuccess, outcome)

	vpcID := defaultVPCID(t, daemon, accountID)
	require.NotEmpty(t, vpcID, "the new account must get a default VPC")

	igw, err := daemon.igwService.AttachmentIntent(t.Context(), accountID, vpcID)
	require.NoError(t, err)
	require.NotNil(t, igw, "the default VPC must have an internet gateway attached")

	rtbs, err := daemon.routeTableService.DescribeRouteTables(t.Context(), &ec2.DescribeRouteTablesInput{}, accountID)
	require.NoError(t, err)
	require.Len(t, rtbs.RouteTables, 1)
	var defaultRoutes int
	for _, r := range rtbs.RouteTables[0].Routes {
		if aws.StringValue(r.DestinationCidrBlock) == "0.0.0.0/0" {
			defaultRoutes++
			assert.Equal(t, aws.StringValue(igw.InternetGatewayId), aws.StringValue(r.GatewayId))
		}
	}
	assert.Equal(t, 1, defaultRoutes, "the default VPC must get its 0.0.0.0/0 route")
}

func TestHandleAccountCreated_RejectsUnusableEvents(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"malformed json", []byte(`{"account_id":`)},
		{"empty account id", []byte(`{"account_id":""}`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			daemon, _ := createDefaultVPCTestDaemon(t)

			outcome := daemon.handleAccountCreated(&nats.Msg{Subject: "account.created", Data: tt.data})
			assert.Equal(t, outcomeError, outcome)

			// Nothing may be provisioned off an event the daemon could not read.
			out, err := daemon.vpcService.DescribeVpcs(t.Context(), &ec2.DescribeVpcsInput{}, "")
			require.NoError(t, err)
			assert.Empty(t, out.Vpcs)
		})
	}
}

func TestHandleEnsureDefaultVpc_RepliesWithVpcIDAndIsIdempotent(t *testing.T) {
	daemon, nc := createDefaultVPCTestDaemon(t)

	sub, err := nc.Subscribe("ec2.test.EnsureDefaultVpc", func(msg *nats.Msg) {
		daemon.handleEnsureDefaultVpc(msg)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()
	require.NoError(t, nc.Flush())

	const accountID = "000000000088"
	req, err := json.Marshal(map[string]string{"account_id": accountID})
	require.NoError(t, err)

	var first struct {
		VpcID string `json:"vpc_id"`
		Error string `json:"error"`
	}
	reply, err := nc.Request("ec2.test.EnsureDefaultVpc", req, 10*time.Second)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(reply.Data, &first))
	assert.Empty(t, first.Error)
	require.NotEmpty(t, first.VpcID)
	assert.Equal(t, defaultVPCID(t, daemon, accountID), first.VpcID)

	// A caller that races the account.created event must see the same VPC.
	var second struct {
		VpcID string `json:"vpc_id"`
		Error string `json:"error"`
	}
	reply, err = nc.Request("ec2.test.EnsureDefaultVpc", req, 10*time.Second)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(reply.Data, &second))
	assert.Equal(t, first.VpcID, second.VpcID, "EnsureDefaultVpc must not create a second default VPC")

	igws, err := daemon.igwService.DescribeInternetGateways(t.Context(), &ec2.DescribeInternetGatewaysInput{}, accountID)
	require.NoError(t, err)
	assert.Len(t, igws.InternetGateways, 1, "a second gateway was attached to the same VPC")
}

func TestHandleEnsureDefaultVpc_ErrorEnvelopes(t *testing.T) {
	tests := []struct {
		name       string
		data       []byte
		nilService bool
		wantError  string
	}{
		{name: "malformed json", data: []byte(`{"account_id":`), wantError: "malformed request"},
		{name: "empty account id", data: []byte(`{"account_id":""}`), wantError: "empty account ID"},
		{name: "no vpc service", data: []byte(`{"account_id":"000000000099"}`), nilService: true, wantError: "VPC service unavailable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			daemon, nc := createDefaultVPCTestDaemon(t)
			if tt.nilService {
				daemon.vpcService = nil
			}

			subject := "ec2.test.EnsureDefaultVpc." + t.Name()
			sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
				assert.Equal(t, outcomeError, daemon.handleEnsureDefaultVpc(msg))
			})
			require.NoError(t, err)
			defer sub.Unsubscribe()
			require.NoError(t, nc.Flush())

			reply, err := nc.Request(subject, tt.data, 10*time.Second)
			require.NoError(t, err, "the caller must always get a reply, never a timeout")

			var got struct {
				VpcID string `json:"vpc_id"`
				Error string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(reply.Data, &got))
			assert.Equal(t, tt.wantError, got.Error)
			assert.Empty(t, got.VpcID)
		})
	}
}

func TestEnsureDefaultVPCInfrastructure_SkipsHalfBuiltAccounts(t *testing.T) {
	daemon, _ := createDefaultVPCTestDaemon(t)

	adminAccount := admin.DefaultAccountID()
	for _, accountID := range []string{utils.GlobalAccountID, adminAccount} {
		_, err := daemon.vpcService.EnsureDefaultVPC(accountID)
		require.NoError(t, err)
	}

	// The admin account is skipped, so only the global account gets a gateway.
	daemon.ensureDefaultVPCInfrastructure(map[string]struct{}{adminAccount: {}})

	skipped, err := daemon.igwService.DescribeInternetGateways(t.Context(), &ec2.DescribeInternetGatewaysInput{}, adminAccount)
	require.NoError(t, err)
	assert.Empty(t, skipped.InternetGateways, "a skipped account must not have infrastructure attached")

	built, err := daemon.igwService.DescribeInternetGateways(t.Context(), &ec2.DescribeInternetGatewaysInput{}, utils.GlobalAccountID)
	require.NoError(t, err)
	assert.Len(t, built.InternetGateways, 1)

	// A later pass with nothing skipped completes the admin account.
	daemon.ensureDefaultVPCInfrastructure(nil)

	completed, err := daemon.igwService.DescribeInternetGateways(t.Context(), &ec2.DescribeInternetGatewaysInput{}, adminAccount)
	require.NoError(t, err)
	assert.Len(t, completed.InternetGateways, 1)
}
