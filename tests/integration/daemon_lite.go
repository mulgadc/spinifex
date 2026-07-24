//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_ec2_account "github.com/mulgadc/spinifex/spinifex/handlers/ec2/account"
	handlers_ec2_eigw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eigw"
	handlers_ec2_igw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/igw"
	handlers_ec2_key "github.com/mulgadc/spinifex/spinifex/handlers/ec2/key"
	handlers_ec2_routetable "github.com/mulgadc/spinifex/spinifex/handlers/ec2/routetable"
	handlers_ec2_tags "github.com/mulgadc/spinifex/spinifex/handlers/ec2/tags"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// testPredastoreBucket is the bucket name key/tags hand to their object
// stores. StartDaemonLite backs both with objectstore.NewMemoryObjectStore,
// so the name is never resolved against a real Predastore — it only needs to
// be non-empty and stable across a test's key/tags calls.
const testPredastoreBucket = "integration-test-bucket"

// DaemonLite is a minimal in-process stand-in for a live spinifex daemon. It
// subscribes the REAL key/tags/route-table/VPC-subnet-SG/IGW service
// implementations — the same production code a live daemon runs — to the
// NATS subjects the gateway's handlers_ec2_*.NewNATS*Service clients call,
// so a test exercises genuine daemon-side business logic instead of a
// StubSubject canned reply.
//
// Scope is deliberately narrow: only resources whose service impl never
// calls viperblock.New are wired here (key, tags, route table, VPC/subnet/SG,
// IGW, EIGW, account settings). Instance lifecycle (needs vm.Manager + QEMU),
// volume/snapshot/image creation (construct viperblock inline), and anything
// OVN-backed are not wired — those need real provisioning DaemonLite
// intentionally avoids.
type DaemonLite struct {
	Key             *handlers_ec2_key.KeyServiceImpl
	Tags            *handlers_ec2_tags.TagsServiceImpl
	VPC             *handlers_ec2_vpc.VPCServiceImpl
	RouteTable      *handlers_ec2_routetable.RouteTableServiceImpl
	IGW             *handlers_ec2_igw.IGWServiceImpl
	EIGW            *handlers_ec2_eigw.EgressOnlyIGWServiceImpl
	AccountSettings *handlers_ec2_account.AccountSettingsServiceImpl

	// MemStore backs Key and Tags — exposed so a test can seed or inspect
	// stored objects directly without going through NATS.
	MemStore *objectstore.MemoryObjectStore
}

// daemonLiteOpts records which of StartDaemonLite's defaults a caller has
// turned off.
type daemonLiteOpts struct {
	// stubVPCD installs the canned vpc.create-sg/vpc.delete-sg acks. On by
	// default; StartVPCDLite is the only reason to turn it off.
	stubVPCD bool
}

// DaemonLiteOption customises what StartDaemonLite wires.
type DaemonLiteOption func(*daemonLiteOpts)

// WithRealVPCD suppresses the canned vpc.create-sg/vpc.delete-sg acks so a
// real subscriber wired by StartVPCDLite answers them instead. Without it the
// stub and the subscriber both reply to the same request and whichever lands
// first wins, so a test would be racing its own fake.
//
// StartVPCDLite must run BEFORE the StartDaemonLite it is paired with:
// StartDaemonLite calls EnsureDefaultVPC, which requests vpc.create-sg
// synchronously and would time out with nothing subscribed.
func WithRealVPCD() DaemonLiteOption {
	return func(o *daemonLiteOpts) { o.stubVPCD = false }
}

// StartDaemonLite constructs the in-scope service impls against gw.NATSConn
// (memory-backed for key/tags, embedded-JetStream-backed for VPC/route
// table/IGW — the same wiring pattern as
// daemon/daemon_handlers_test.go:createFullTestDaemonWithStore and
// daemon/daemon_wire_lb_test.go:newSubscribeTestDaemon) and subscribes them to
// every subject those five resources answer on a live daemon. Every
// subscription is torn down via t.Cleanup.
//
// Must be called before a test issues any request for a subject it wires —
// StubSubject and StartDaemonLite must never cover the same subject in one
// test, since NATS would deliver the request to both plain subscribers and
// whichever responds first wins the race.
func StartDaemonLite(t *testing.T, gw *Gateway, opts ...DaemonLiteOption) *DaemonLite {
	t.Helper()

	o := daemonLiteOpts{stubVPCD: true}
	for _, apply := range opts {
		apply(&o)
	}

	nc := gw.NATSConn
	memStore := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{
		AZ:         testAZ,
		Predastore: config.PredastoreConfig{Bucket: testPredastoreBucket},
	}

	keySvc := handlers_ec2_key.NewKeyServiceImplWithStore(memStore, cfg.Predastore.Bucket)
	tagsSvc := handlers_ec2_tags.NewTagsServiceImplWithStore(cfg, memStore)

	vpcSvc, err := handlers_ec2_vpc.NewVPCServiceImplWithNATS(t.Context(), cfg, nc)
	require.NoError(t, err, "construct VPC service")

	rtbSvc, err := handlers_ec2_routetable.NewRouteTableServiceImplWithNATS(t.Context(), cfg, nc)
	require.NoError(t, err, "construct route table service")

	igwSvc, err := handlers_ec2_igw.NewIGWServiceImplWithNATS(t.Context(), cfg, nc)
	require.NoError(t, err, "construct IGW service")

	eigwSvc, err := handlers_ec2_eigw.NewEgressOnlyIGWServiceImplWithNATS(t.Context(), cfg, nc)
	require.NoError(t, err, "construct EIGW service")

	acctSettingsSvc, err := handlers_ec2_account.NewAccountSettingsServiceImplWithNATS(t.Context(), cfg, nc)
	require.NoError(t, err, "construct account settings service")

	dl := &DaemonLite{
		Key:             keySvc,
		Tags:            tagsSvc,
		VPC:             vpcSvc,
		RouteTable:      rtbSvc,
		IGW:             igwSvc,
		EIGW:            eigwSvc,
		AccountSettings: acctSettingsSvc,
		MemStore:        memStore,
	}

	// CreateVpc/EnsureDefaultVPC/DeleteVpc synchronously round-trip through
	// vpcd (the OVN topology-translation daemon) to provision/tear down each
	// VPC's default security group (handlers/ec2/vpc/security_group.go
	// createDefaultSecurityGroupInternal/deleteSecurityGroupInternal ->
	// requestSGEvent -> utils.RequestEvent). vpcd itself is out of scope for
	// this tier (it's an external OVN process, not a key/tags/routetable/vpc
	// service impl), so it is stubbed here exactly like any other
	// out-of-scope daemon-side responder: a fixed {"success":true} ack on
	// "vpc.create-sg"/"vpc.delete-sg", satisfying utils.RequestEvent's
	// {success,error} reply contract. The SG record itself is written to the
	// KV store by the real service impl before this event is even sent, so
	// stubbing the vpcd ack never substitutes for in-scope logic under test —
	// it only unblocks the synchronous call so CreateVpc/DeleteVpc can
	// complete instead of failing with ServerInternal on every VPC creation.
	//
	// WithRealVPCD skips both, for the tests that wire a genuine subscriber
	// over a real OVN NB DB (StartVPCDLite) and assert on the rows it writes.
	if o.stubVPCD {
		gw.StubSubject(t, "vpc.create-sg", []byte(`{"success":true}`))
		gw.StubSubject(t, "vpc.delete-sg", []byte(`{"success":true}`))
	}

	// A live daemon creates the account's default VPC by reacting to the
	// "iam.account.created" event SeedBootstrap publishes at gateway startup
	// (daemon/daemon_handlers_vpc.go handleAccountCreated). StartGateway has
	// already published that event by the time this function runs, so
	// subscribing to it here would never fire — call the same idempotent
	// EnsureDefaultVPC the handler calls directly instead. IGW auto-attach
	// (ensureDefaultVPCInfrastructureFor) is not replicated: the default VPC
	// is left with no IGW, which the ported route-table test already treats
	// as a soft, non-fatal condition.
	_, err = dl.VPC.EnsureDefaultVPC(gw.AccountID)
	require.NoError(t, err, "EnsureDefaultVPC for %s", gw.AccountID)

	dl.subscribe(t, nc)

	return dl
}

// dispatch replicates daemon.handleNATSRequest's unmarshal -> service ->
// marshal -> respond envelope (daemon/daemon_handlers.go), so a subscribed
// service impl answers a NATS request exactly like the real daemon handler
// would, without needing an exported hook into the unexported daemon package.
func dispatch[I any, O any](msg *nats.Msg, serviceFn func(context.Context, *I, string) (*O, error)) {
	ctx, span := utils.StartConsumerSpan(msg)
	defer span.End()

	accountID := utils.AccountIDFromMsg(msg)
	input := new(I)
	if errResp := utils.UnmarshalJsonPayload(input, msg.Data); errResp != nil {
		utils.MarkSpanError(span, errors.New(awserrors.ErrorInvalidParameterValue))
		if err := msg.Respond(errResp); err != nil {
			slog.Error("dispatch: failed to respond to NATS request", "err", err)
		}
		return
	}

	output, err := serviceFn(ctx, input, accountID)
	if err != nil {
		utils.MarkSpanError(span, err)
		if respErr := msg.Respond(utils.GenerateErrorPayload(awserrors.ValidErrorCode(err.Error()))); respErr != nil {
			slog.Error("dispatch: failed to respond to NATS request", "err", respErr)
		}
		return
	}

	jsonResponse, err := json.Marshal(output)
	if err != nil {
		slog.Error("dispatch: failed to marshal response", "err", err)
		if respErr := msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorServerInternal)); respErr != nil {
			slog.Error("dispatch: failed to respond to NATS request", "err", respErr)
		}
		return
	}
	if err := msg.Respond(jsonResponse); err != nil {
		slog.Error("dispatch: failed to respond to NATS request", "err", err)
	}
}

// sub registers a plain (non-queue-group) subscription and its t.Cleanup
// unsubscribe — DaemonLite only ever runs one subscriber per subject in a
// given test, so there's no fan-out to coordinate.
func sub(t *testing.T, nc *nats.Conn, subject string, handler nats.MsgHandler) {
	t.Helper()
	s, err := nc.Subscribe(subject, handler)
	require.NoError(t, err, "subscribe %s", subject)
	t.Cleanup(func() { _ = s.Unsubscribe() })
}

// subscribe wires every subject the in-scope ported tests (TestKeyPairs,
// TestTagManagement, TestRouteTableValidation, TestReplaceRouteConvergence,
// TestAccountScoping_*, TestSerialConsoleAccess) exercise, plus their
// supporting VPC/subnet/SG/IGW/EIGW/account-settings subjects, to the real
// service impls held on dl.
func (dl *DaemonLite) subscribe(t *testing.T, nc *nats.Conn) {
	t.Helper()

	// Key pairs.
	sub(t, nc, "ec2.CreateKeyPair", func(m *nats.Msg) { dispatch(m, dl.Key.CreateKeyPair) })
	sub(t, nc, "ec2.DeleteKeyPair", func(m *nats.Msg) { dispatch(m, dl.Key.DeleteKeyPair) })
	sub(t, nc, "ec2.DescribeKeyPairs", func(m *nats.Msg) { dispatch(m, dl.Key.DescribeKeyPairs) })
	sub(t, nc, "ec2.ImportKeyPair", func(m *nats.Msg) { dispatch(m, dl.Key.ImportKeyPair) })

	// Tags. Unlike the live daemon's handleEC2CreateTags/handleEC2DeleteTags
	// (daemon/daemon_handlers_tag.go), this dispatches straight to
	// TagsServiceImpl and skips the instance-ID routing split and the
	// recordTagMirrors projection onto owning resource records — both exist
	// only to support instance/volume tagging, which is out of scope here
	// (see mulga-xaa9f), and neither is observed by the ported
	// TestTagManagement assertions (all of which read back through
	// DescribeTags, the central tag store both paths write identically).
	sub(t, nc, "ec2.CreateTags", func(m *nats.Msg) { dispatch(m, dl.Tags.CreateTags) })
	sub(t, nc, "ec2.DeleteTags", func(m *nats.Msg) { dispatch(m, dl.Tags.DeleteTags) })
	sub(t, nc, "ec2.DescribeTags", func(m *nats.Msg) { dispatch(m, dl.Tags.DescribeTags) })

	// Route tables.
	sub(t, nc, "ec2.CreateRouteTable", func(m *nats.Msg) { dispatch(m, dl.RouteTable.CreateRouteTable) })
	sub(t, nc, "ec2.DeleteRouteTable", func(m *nats.Msg) { dispatch(m, dl.RouteTable.DeleteRouteTable) })
	sub(t, nc, "ec2.DescribeRouteTables", func(m *nats.Msg) { dispatch(m, dl.RouteTable.DescribeRouteTables) })
	sub(t, nc, "ec2.CreateRoute", func(m *nats.Msg) { dispatch(m, dl.RouteTable.CreateRoute) })
	sub(t, nc, "ec2.DeleteRoute", func(m *nats.Msg) { dispatch(m, dl.RouteTable.DeleteRoute) })
	sub(t, nc, "ec2.ReplaceRoute", func(m *nats.Msg) { dispatch(m, dl.RouteTable.ReplaceRoute) })
	sub(t, nc, "ec2.AssociateRouteTable", func(m *nats.Msg) { dispatch(m, dl.RouteTable.AssociateRouteTable) })
	sub(t, nc, "ec2.DisassociateRouteTable", func(m *nats.Msg) { dispatch(m, dl.RouteTable.DisassociateRouteTable) })
	sub(t, nc, "ec2.ReplaceRouteTableAssociation", func(m *nats.Msg) { dispatch(m, dl.RouteTable.ReplaceRouteTableAssociation) })

	// VPC / subnet / ENI / security group.
	sub(t, nc, "ec2.CreateVpc", func(m *nats.Msg) { dispatch(m, dl.VPC.CreateVpc) })
	sub(t, nc, "ec2.DeleteVpc", func(m *nats.Msg) { dispatch(m, dl.VPC.DeleteVpc) })
	sub(t, nc, "ec2.DescribeVpcs", func(m *nats.Msg) { dispatch(m, dl.VPC.DescribeVpcs) })
	sub(t, nc, "ec2.CreateSubnet", func(m *nats.Msg) { dispatch(m, dl.VPC.CreateSubnet) })
	sub(t, nc, "ec2.DeleteSubnet", func(m *nats.Msg) { dispatch(m, dl.VPC.DeleteSubnet) })
	sub(t, nc, "ec2.DescribeSubnets", func(m *nats.Msg) { dispatch(m, dl.VPC.DescribeSubnets) })
	sub(t, nc, "ec2.ModifySubnetAttribute", func(m *nats.Msg) { dispatch(m, dl.VPC.ModifySubnetAttribute) })
	sub(t, nc, "ec2.ModifyVpcAttribute", func(m *nats.Msg) { dispatch(m, dl.VPC.ModifyVpcAttribute) })
	sub(t, nc, "ec2.DescribeVpcAttribute", func(m *nats.Msg) { dispatch(m, dl.VPC.DescribeVpcAttribute) })
	sub(t, nc, "ec2.CreateNetworkInterface", func(m *nats.Msg) { dispatch(m, dl.VPC.CreateNetworkInterface) })
	sub(t, nc, "ec2.DeleteNetworkInterface", func(m *nats.Msg) { dispatch(m, dl.VPC.DeleteNetworkInterface) })
	sub(t, nc, "ec2.DescribeNetworkInterfaces", func(m *nats.Msg) { dispatch(m, dl.VPC.DescribeNetworkInterfaces) })
	sub(t, nc, "ec2.ModifyNetworkInterfaceAttribute", func(m *nats.Msg) { dispatch(m, dl.VPC.ModifyNetworkInterfaceAttribute) })
	sub(t, nc, "ec2.CreateSecurityGroup", func(m *nats.Msg) { dispatch(m, dl.VPC.CreateSecurityGroup) })
	sub(t, nc, "ec2.DeleteSecurityGroup", func(m *nats.Msg) { dispatch(m, dl.VPC.DeleteSecurityGroup) })
	sub(t, nc, "ec2.DescribeSecurityGroups", func(m *nats.Msg) { dispatch(m, dl.VPC.DescribeSecurityGroups) })
	sub(t, nc, "ec2.DescribeSecurityGroupRules", func(m *nats.Msg) { dispatch(m, dl.VPC.DescribeSecurityGroupRules) })
	sub(t, nc, "ec2.AuthorizeSecurityGroupIngress", func(m *nats.Msg) { dispatch(m, dl.VPC.AuthorizeSecurityGroupIngress) })
	sub(t, nc, "ec2.AuthorizeSecurityGroupEgress", func(m *nats.Msg) { dispatch(m, dl.VPC.AuthorizeSecurityGroupEgress) })
	sub(t, nc, "ec2.RevokeSecurityGroupIngress", func(m *nats.Msg) { dispatch(m, dl.VPC.RevokeSecurityGroupIngress) })
	sub(t, nc, "ec2.RevokeSecurityGroupEgress", func(m *nats.Msg) { dispatch(m, dl.VPC.RevokeSecurityGroupEgress) })

	// Internet gateways.
	sub(t, nc, "ec2.CreateInternetGateway", func(m *nats.Msg) { dispatch(m, dl.IGW.CreateInternetGateway) })
	sub(t, nc, "ec2.DeleteInternetGateway", func(m *nats.Msg) { dispatch(m, dl.IGW.DeleteInternetGateway) })
	sub(t, nc, "ec2.DescribeInternetGateways", func(m *nats.Msg) { dispatch(m, dl.IGW.DescribeInternetGateways) })
	sub(t, nc, "ec2.AttachInternetGateway", func(m *nats.Msg) { dispatch(m, dl.IGW.AttachInternetGateway) })
	sub(t, nc, "ec2.DetachInternetGateway", func(m *nats.Msg) { dispatch(m, dl.IGW.DetachInternetGateway) })

	// Egress-only internet gateways.
	sub(t, nc, "ec2.CreateEgressOnlyInternetGateway", func(m *nats.Msg) { dispatch(m, dl.EIGW.CreateEgressOnlyInternetGateway) })
	sub(t, nc, "ec2.DeleteEgressOnlyInternetGateway", func(m *nats.Msg) { dispatch(m, dl.EIGW.DeleteEgressOnlyInternetGateway) })
	sub(t, nc, "ec2.DescribeEgressOnlyInternetGateways", func(m *nats.Msg) { dispatch(m, dl.EIGW.DescribeEgressOnlyInternetGateways) })

	// Account settings.
	sub(t, nc, "ec2.EnableEbsEncryptionByDefault", func(m *nats.Msg) { dispatch(m, dl.AccountSettings.EnableEbsEncryptionByDefault) })
	sub(t, nc, "ec2.DisableEbsEncryptionByDefault", func(m *nats.Msg) { dispatch(m, dl.AccountSettings.DisableEbsEncryptionByDefault) })
	sub(t, nc, "ec2.GetEbsEncryptionByDefault", func(m *nats.Msg) { dispatch(m, dl.AccountSettings.GetEbsEncryptionByDefault) })
	sub(t, nc, "ec2.EnableSerialConsoleAccess", func(m *nats.Msg) { dispatch(m, dl.AccountSettings.EnableSerialConsoleAccess) })
	sub(t, nc, "ec2.DisableSerialConsoleAccess", func(m *nats.Msg) { dispatch(m, dl.AccountSettings.DisableSerialConsoleAccess) })
	sub(t, nc, "ec2.GetSerialConsoleAccessStatus", func(m *nats.Msg) { dispatch(m, dl.AccountSettings.GetSerialConsoleAccessStatus) })
}
