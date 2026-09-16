package gateway_ecs

import (
	"bytes"
	"encoding/json"
	"log/slog"

	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// availabilityZoneRebalancing is a field the pinned aws-sdk-go's ECS shapes
// predate: v1.55.8 is the last v1 release and carries no such member, so it can
// be neither parsed into an input nor set on an output through the SDK types.
// It is read off the raw request body and written onto the encoded response
// instead.
const availabilityZoneRebalancingField = "availabilityZoneRebalancing"

// azRebalancingDisabled is the only value this platform can report honestly.
// Nothing here rebalances tasks across availability zones, so a service always
// reads DISABLED and a request to enable it is refused rather than stored.
const azRebalancingDisabled = "DISABLED"

// checkAZRebalancing refuses a CreateService or UpdateService asking for
// availability-zone rebalancing. Terraform sends the attribute on every apply
// once it is in state, so silently dropping ENABLED would report a service as
// rebalancing when nothing does.
func checkAZRebalancing(body []byte) error {
	if len(body) == 0 {
		return nil
	}
	var req struct {
		AvailabilityZoneRebalancing *string `json:"availabilityZoneRebalancing"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		// A body the SDK shapes will reject anyway; let that error be the one
		// the caller sees rather than a second opinion from here.
		return nil //nolint:nilerr // unmarshalIfBody reports the real parse error
	}
	if req.AvailabilityZoneRebalancing == nil || *req.AvailabilityZoneRebalancing == azRebalancingDisabled {
		return nil
	}
	return awserrors.Errorf(awserrors.ErrorECSInvalidParameter,
		"availabilityZoneRebalancing %s cannot be applied (no cross-zone rebalancer)",
		*req.AvailabilityZoneRebalancing)
}

// addSDKGapFields re-encodes a service-carrying response with the fields the
// pinned SDK's shapes do not define. Embedding ecs.Service in a wrapper does not
// work: jsonutil nests an embedded struct rather than flattening it, so the
// service would arrive under its own key. Mirroring the whole shape to carry one
// string would duplicate forty fields, so the field is added to the encoded body.
// A response carrying no service is returned untouched.
func addSDKGapFields(obj any, body []byte) []byte {
	var key string
	switch obj.(type) {
	case *ecs.DescribeServicesOutput:
		key = "services"
	case *ecs.CreateServiceOutput, *ecs.UpdateServiceOutput, *ecs.DeleteServiceOutput:
		key = "service"
	default:
		return body
	}

	// UseNumber keeps the epoch-second times jsonutil emitted as they were
	// written; decoding them into float64 would re-encode them in exponent form.
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		slog.Error("ECS: failed to decode response for field injection", "err", err)
		return body
	}

	switch v := doc[key].(type) {
	case map[string]any:
		setAZRebalancing(v)
	case []any:
		for _, item := range v {
			if svc, ok := item.(map[string]any); ok {
				setAZRebalancing(svc)
			}
		}
	default:
		return body
	}

	patched, err := json.Marshal(doc)
	if err != nil {
		slog.Error("ECS: failed to re-encode response after field injection", "err", err)
		return body
	}
	return patched
}

// setAZRebalancing adds the field to one service object, leaving a value the
// daemon already supplied alone so this never overwrites a real answer.
func setAZRebalancing(svc map[string]any) {
	if _, ok := svc[availabilityZoneRebalancingField]; ok {
		return
	}
	svc[availabilityZoneRebalancingField] = azRebalancingDisabled
}
