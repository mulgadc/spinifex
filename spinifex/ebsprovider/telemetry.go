package ebsprovider

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/mulgadc/spinifex/spinifex/ebsprovider"

// spanNamePrefix puts provider spans in the same namespace as the ebs.mount
// and ebs.unmount spans the legacy path emits.
const spanNamePrefix = "ebs."

const (
	attrSubject     = "ebs.provider.subject"
	attrVolumeID    = "ebs.provider.volume_id"
	attrNodeID      = "ebs.provider.node_id"
	attrOwnerRouted = "ebs.provider.owner_routed"
	attrErrorCode   = "ebs.provider.error_code"
)

// VerbUnknown is the verb reported for a subject that is not part of the
// contract. Unrecognised subjects collapse to it rather than becoming span
// names of their own, which is what keeps the name set bounded.
const VerbUnknown = "unknown"

const (
	verbCapabilities    = "capabilities"
	verbVolumeCreate    = "volume.create"
	verbVolumeList      = "volume.list"
	verbVolumeDelete    = "volume.delete"
	verbVolumePublish   = "volume.publish"
	verbVolumeUnpublish = "volume.unpublish"
	verbSnapshotDelete  = "snapshot.delete"
	verbSnapshotList    = "snapshot.list"
)

// subjectVerbs maps every fixed subject to its verb. The dynamic subject
// families (owner-routed, snapshot create, per-node mount) are handled by
// subjectTarget, which strips the ID token before naming the verb.
var subjectVerbs = map[string]string{
	CapabilitiesSubject:   verbCapabilities,
	CreateVolumeSubject:   verbVolumeCreate,
	GetVolumeSubject:      verbVolumeDescribe,
	ListVolumesSubject:    verbVolumeList,
	ExpandVolumeSubject:   verbVolumeExpand,
	DeleteVolumeSubject:   verbVolumeDelete,
	DeleteSnapshotSubject: verbSnapshotDelete,
	CopySnapshotSubject:   verbSnapshotCopy,
	ListSnapshotsSubject:  verbSnapshotList,
}

// ownerVerbs is the closed set OwnerSubject can build. Parsed subjects are
// checked against it so a malformed subject cannot invent a verb.
var ownerVerbs = map[string]bool{
	verbSnapshotCreate: true,
	verbSnapshotCopy:   true,
	verbVolumeExpand:   true,
	verbVolumeDescribe: true,
}

// subjectTarget describes what a subject addresses. The verb is drawn from a
// closed set and is safe to use as a span name; volumeID and nodeID are
// unbounded and belong in attributes only.
type subjectTarget struct {
	verb        string
	volumeID    string
	nodeID      string
	ownerRouted bool
}

// SubjectVerb reports the operation subject names, with any embedded volume
// or node ID removed. A subject outside the contract reports VerbUnknown.
func SubjectVerb(subject string) string {
	return parseSubject(subject).verb
}

func parseSubject(subject string) subjectTarget {
	if verb, ok := subjectVerbs[subject]; ok {
		return subjectTarget{verb: verb}
	}
	if volumeID, verb, ok := ParseOwnerSubject(subject); ok && ownerVerbs[verb] {
		return subjectTarget{verb: verb, volumeID: volumeID, ownerRouted: true}
	}
	if volumeID, ok := strings.CutPrefix(subject, SnapshotCreateSubjectPrefix); ok && validateSubjectToken(volumeID) == nil {
		return subjectTarget{verb: verbSnapshotCreate, volumeID: volumeID}
	}
	rest, ok := strings.CutPrefix(subject, subjectPrefix)
	if !ok {
		return subjectTarget{verb: VerbUnknown}
	}
	nodeID, suffix, ok := strings.Cut(rest, ".")
	if !ok || validateSubjectToken(nodeID) != nil {
		return subjectTarget{verb: VerbUnknown}
	}
	switch suffix {
	case "mount":
		return subjectTarget{verb: verbVolumePublish, nodeID: nodeID}
	case "unmount":
		return subjectTarget{verb: verbVolumeUnpublish, nodeID: nodeID}
	default:
		return subjectTarget{verb: VerbUnknown}
	}
}

func (t subjectTarget) attributes(subject string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("messaging.system", "nats"),
		attribute.String(attrSubject, subject),
	}
	if t.volumeID != "" {
		attrs = append(attrs, attribute.String(attrVolumeID, t.volumeID))
	}
	if t.nodeID != "" {
		attrs = append(attrs, attribute.String(attrNodeID, t.nodeID))
	}
	if t.ownerRouted {
		attrs = append(attrs, attribute.Bool(attrOwnerRouted, true))
	}
	return attrs
}

var _ propagation.TextMapCarrier = (*headerCarrier)(nil)

// headerCarrier adapts nats.Header to the OTel carrier. It is a copy of the
// one in spinifex/utils rather than an import: this package is the wire
// contract and must not depend on the control plane's helper tree.
type headerCarrier nats.Header

func (c headerCarrier) Get(key string) string { return nats.Header(c).Get(key) }
func (c headerCarrier) Set(key, value string) { nats.Header(c).Set(key, value) }
func (c headerCarrier) Keys() []string {
	return slices.Collect(maps.Keys(c))
}

// startClientSpan opens a span for an outbound request and injects the trace
// context into hdr so the serving provider joins the same trace.
func startClientSpan(ctx context.Context, subject string, hdr nats.Header) (context.Context, trace.Span) {
	target := parseSubject(subject)
	ctx, span := otel.Tracer(tracerName).Start(ctx, spanNamePrefix+target.verb,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(target.attributes(subject)...))
	otel.GetTextMapPropagator().Inject(ctx, headerCarrier(hdr))
	return ctx, span
}

// startCompletionSpan covers the wait for an asynchronous completion publish.
// A snapshot spends nearly all its time here, and the request span for the
// accept leg ends long before it.
func startCompletionSpan(ctx context.Context, verb, completionSubject string) (context.Context, trace.Span) {
	return otel.Tracer(tracerName).Start(ctx, spanNamePrefix+verb+".wait",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "nats"),
			attribute.String(attrSubject, completionSubject),
		))
}

// StartServerSpan joins the trace carried in msg's headers and opens a span
// for the verb its subject names. Callers must End the returned span and
// should pass the returned context into the work the handler does.
func StartServerSpan(ctx context.Context, msg *nats.Msg) (context.Context, trace.Span) {
	target := parseSubject(msg.Subject)
	if msg.Header != nil {
		ctx = otel.GetTextMapPropagator().Extract(ctx, headerCarrier(msg.Header))
	}
	return otel.Tracer(tracerName).Start(ctx, spanNamePrefix+target.verb,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(target.attributes(msg.Subject)...))
}

// RecordSpanError marks span failed for a transport or handler error.
func RecordSpanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// RecordProviderError tags span with the wire error code a handler is about
// to return, so a failed operation is visible as an error in the trace and
// not merely as a successful round trip.
func RecordProviderError(span trace.Span, providerErr *ProviderError) {
	if providerErr == nil {
		return
	}
	span.SetAttributes(attribute.String(attrErrorCode, string(providerErr.Code)))
	span.SetStatus(codes.Error, providerErr.Message)
}

// RecordResponseError tags span with the error an encoded response carries.
// The response types share no accessor for it, so this re-reads the one field
// rather than making every verb implement one; it runs only when recording.
func RecordResponseError(span trace.Span, data []byte) {
	if !span.IsRecording() {
		return
	}
	var envelope struct {
		Error *ProviderError `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return
	}
	RecordProviderError(span, envelope.Error)
}
