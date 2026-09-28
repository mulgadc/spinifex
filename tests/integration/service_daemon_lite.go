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
	handlers_acm "github.com/mulgadc/spinifex/spinifex/handlers/acm"
	handlers_ecs "github.com/mulgadc/spinifex/spinifex/handlers/ecs"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// StartServiceDaemonLite subscribes the real ACM, ECS, EKS, ELBv2 and RDS
// services — the ones a live daemon constructs in daemon.go — to their
// "<service>.<Method>" subjects, with no provisioning backends behind them.
// Requests are validated and stored as on a live daemon; anything that would
// launch an instance or a VM fails at the NATS subject nothing answers.
func StartServiceDaemonLite(t *testing.T, gw *Gateway) {
	t.Helper()
	nc := gw.NATSConn
	cfg := &config.Config{AZ: testAZ, Region: testRegion}
	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)

	acm, err := handlers_acm.NewACMServiceImplWithNATS(t.Context(), cfg, nc, masterKey)
	require.NoError(t, err, "construct ACM service")
	subscribeServiceMethods(t, nc, "acm", acm)

	elbv2, err := handlers_elbv2.NewELBv2ServiceImplWithNATS(cfg, nc, masterKey)
	require.NoError(t, err, "construct ELBv2 service")
	t.Cleanup(elbv2.Close)
	subscribeServiceMethods(t, nc, "elbv2", elbv2)

	eks, err := handlers_eks.NewEKSServiceImpl(handlers_eks.EKSServiceDeps{
		Config: cfg, NATSConn: nc, MasterKey: masterKey, Region: testRegion, HolderID: "integration-test-node", ClusterSize: 1,
	})
	require.NoError(t, err, "construct EKS service")
	t.Cleanup(eks.Shutdown)
	subscribeServiceMethods(t, nc, "eks", eks)

	subscribeServiceMethods(t, nc, "ecs", handlers_ecs.NewService(nc, testRegion, ""))
	subscribeServiceMethods(t, nc, "rds", handlers_rds.NewService(nc, testRegion))
}

var (
	contextType = reflect.TypeFor[context.Context]()
	errorType   = reflect.TypeFor[error]()
	stringType  = reflect.TypeFor[string]()
)

// subscribeServiceMethods subscribes every method of service shaped like a
// daemon NATS handler — (ctx, *Input, accountID[, principalARN]) (*Output,
// error) — to "<prefix>.<Method>", replicating daemon.handleNATSRequest.
// Deriving the subjects from the methods keeps this from drifting from the
// daemon's hand-written subscription lists.
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
