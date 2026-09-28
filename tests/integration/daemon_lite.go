//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
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
	"github.com/nats-io/nats.go/jetstream"
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

	tagsJS, err := jetstream.New(nc)
	require.NoError(t, err, "jetstream handle for the tag store")
	tagsKV, err := handlers_ec2_tags.GetOrCreateTagsBucket(t.Context(), tagsJS)
	require.NoError(t, err, "tag store bucket")
	tagsSvc := handlers_ec2_tags.NewTagsServiceImplWithStore(cfg, memStore, tagsKV)

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

// dispatch answers a NATS request with one service method, for subjects not
// named after the method.
func dispatch[I any, O any](msg *nats.Msg, serviceFn func(context.Context, *I, string) (*O, error)) {
	dispatchReflected(msg, reflect.ValueOf(serviceFn), false)
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

// subscribe wires every handler method of the service impls held on dl to its
// "ec2.<Method>" subject.
func (dl *DaemonLite) subscribe(t *testing.T, nc *nats.Conn) {
	t.Helper()
	// Tags dispatch straight to TagsServiceImpl, skipping the live daemon's
	// instance-ID routing and tag mirroring, which only instance and volume
	// tagging need; both paths write the central tag store DescribeTags reads.
	for _, service := range []any{dl.Key, dl.Tags, dl.RouteTable, dl.VPC, dl.IGW, dl.EIGW, dl.AccountSettings} {
		subscribeServiceMethods(t, nc, "ec2", service)
	}
}

var (
	contextType = reflect.TypeFor[context.Context]()
	errorType   = reflect.TypeFor[error]()
	stringType  = reflect.TypeFor[string]()
)

// subscribeServiceMethods subscribes each method shaped like a daemon NATS
// handler, (ctx, *Input, accountID[, principalARN]) (*Output, error), to
// "<prefix>.<Method>", so the subjects cannot drift from the service.
func subscribeServiceMethods(t *testing.T, nc *nats.Conn, prefix string, service any) {
	t.Helper()
	value := reflect.ValueOf(service)
	for i := range value.NumMethod() {
		method := value.Type().Method(i)
		handler := value.Method(i)
		withPrincipal, ok := daemonHandlerShape(handler.Type())
		if !ok {
			continue
		}
		sub(t, nc, prefix+"."+method.Name, func(msg *nats.Msg) {
			dispatchReflected(msg, handler, withPrincipal)
		})
	}
}

func daemonHandlerShape(fn reflect.Type) (withPrincipal, ok bool) {
	if fn.NumOut() != 2 || fn.Out(0).Kind() != reflect.Pointer || fn.Out(1) != errorType {
		return false, false
	}
	switch fn.NumIn() {
	case 3:
	case 4:
		if fn.In(3) != stringType {
			return false, false
		}
		withPrincipal = true
	default:
		return false, false
	}
	return withPrincipal, fn.In(0) == contextType && fn.In(1).Kind() == reflect.Pointer && fn.In(2) == stringType
}

func dispatchReflected(msg *nats.Msg, handler reflect.Value, withPrincipal bool) {
	ctx, span := utils.StartConsumerSpan(msg)
	defer span.End()
	ctx = utils.WithIdempotencyKey(ctx, utils.IdempotencyKeyFromMsg(msg))

	input := reflect.New(handler.Type().In(1).Elem())
	if errResp := utils.UnmarshalJsonPayload(input.Interface(), msg.Data); errResp != nil {
		respond(msg, errResp)
		return
	}
	args := []reflect.Value{reflect.ValueOf(ctx), input, reflect.ValueOf(utils.AccountIDFromMsg(msg))}
	if withPrincipal {
		args = append(args, reflect.ValueOf(utils.PrincipalARNFromMsg(msg)))
	}
	results := handler.Call(args)
	if err, _ := results[1].Interface().(error); err != nil {
		utils.MarkSpanError(span, err)
		_, message, _ := awserrors.ResolveErrorDetail(err)
		respond(msg, utils.GenerateErrorPayloadWithMessage(awserrors.ValidErrorCodeFromError(err), message))
		return
	}
	payload, err := json.Marshal(results[0].Interface())
	if err != nil {
		respond(msg, utils.GenerateErrorPayload(awserrors.ErrorServerInternal))
		return
	}
	respond(msg, payload)
}

func respond(msg *nats.Msg, payload []byte) {
	if err := msg.Respond(payload); err != nil {
		slog.Error("service daemon-lite: failed to respond to NATS request", "subject", msg.Subject, "err", err)
	}
}
