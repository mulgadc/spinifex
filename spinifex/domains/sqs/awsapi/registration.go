// Package awsapi is the public SQS adapter: AWS JSON 1.0 with query-compatible
// errors, served under the sqs signing service. It parses and validates each
// request, authorizes it on the queue ARN, then runs the queue owner.
package awsapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
)

// ServiceName is the SigV4 credential-scope service SQS clients sign for.
const ServiceName = "sqs"

// TargetPrefix is the X-Amz-Target prefix of every SQS JSON 1.0 operation.
const TargetPrefix = "AmazonSQS."

// ErrorNamespace is the Smithy namespace of the errors the SQS model defines.
const ErrorNamespace = "com.amazonaws.sqs"

// Operation names the registration dispatches.
const (
	OpCreateQueue           = "CreateQueue"
	OpGetQueueURL           = "GetQueueUrl"
	OpGetQueueAttributes    = "GetQueueAttributes"
	OpListQueues            = "ListQueues"
	OpSetQueueAttributes    = "SetQueueAttributes"
	OpTagQueue              = "TagQueue"
	OpUntagQueue            = "UntagQueue"
	OpListQueueTags         = "ListQueueTags"
	OpSendMessage           = "SendMessage"
	OpReceiveMessage        = "ReceiveMessage"
	OpDeleteMessage         = "DeleteMessage"
	OpChangeVisibility      = "ChangeMessageVisibility"
	OpDeleteMessageBatch    = "DeleteMessageBatch"
	OpSendMessageBatch      = "SendMessageBatch"
	OpChangeVisibilityBatch = "ChangeMessageVisibilityBatch"
)

// Queues is the queue owner capability the actions run against.
type Queues interface {
	Create(ctx context.Context, req queue.CreateRequest) (queue.Record, error)
	Resolve(ctx context.Context, account, region, name string) (queue.Record, error)
	List(ctx context.Context, req queue.ListRequest) (queue.ListResult, error)
	SetAttributes(ctx context.Context, account, region, name string, attrs map[string]string) (queue.Record, error)
	Tag(ctx context.Context, account, region, name string, tags map[string]string) (queue.Record, error)
	Untag(ctx context.Context, account, region, name string, keys []string) (queue.Record, error)
	Tags(ctx context.Context, account, region, name string) (map[string]string, error)
}

var _ Queues = (*queue.Store)(nil)

// Messages is the message owner capability sends run against.
type Messages interface {
	Send(ctx context.Context, in message.SendInput) (message.SendResult, error)
}

var _ Messages = (*message.Sender)(nil)

// Receives is the message owner capability receives run against.
type Receives interface {
	Receive(ctx context.Context, in message.ReceiveInput) ([]message.Received, error)
}

var _ Receives = (*message.Receiver)(nil)

// Deletes is the message owner capability deletes run against.
type Deletes interface {
	Delete(ctx context.Context, in message.DeleteInput) error
}

var _ Deletes = (*message.Deleter)(nil)

// Visibility is the message owner capability visibility changes run against.
type Visibility interface {
	ChangeVisibility(ctx context.Context, in message.ChangeVisibilityInput) error
}

var _ Visibility = (*message.VisibilityChanger)(nil)

// Deps are the capabilities the adapter dispatches to. A nil Queues answers
// every valid, authorized request with ServiceUnavailable.
type Deps struct {
	Queues Queues
	// Messages runs sends; nil answers a valid, authorized send with
	// ServiceUnavailable.
	Messages Messages
	// Receives and Deletes run ReceiveMessage and DeleteMessage; nil answers
	// a valid, authorized request with ServiceUnavailable.
	Receives Receives
	Deletes  Deletes
	// Visibility runs ChangeMessageVisibility; nil answers a valid,
	// authorized request with ServiceUnavailable.
	Visibility Visibility
	// Endpoint is the base URL queue URLs are built on, with no trailing
	// slash, e.g. https://sqs.example:9999.
	Endpoint string
}

type handler func(d Deps, w http.ResponseWriter, inv dispatch.Invocation, body []byte) error

var operations = map[string]handler{
	OpCreateQueue:           createQueue,
	OpGetQueueURL:           getQueueURL,
	OpGetQueueAttributes:    getQueueAttributes,
	OpListQueues:            listQueues,
	OpSetQueueAttributes:    setQueueAttributes,
	OpTagQueue:              tagQueue,
	OpUntagQueue:            untagQueue,
	OpListQueueTags:         listQueueTags,
	OpSendMessage:           sendMessage,
	OpReceiveMessage:        receiveMessage,
	OpDeleteMessage:         deleteMessage,
	OpChangeVisibility:      changeMessageVisibility,
	OpDeleteMessageBatch:    deleteMessageBatch,
	OpSendMessageBatch:      sendMessageBatch,
	OpChangeVisibilityBatch: changeMessageVisibilityBatch,
}

// OperationNames lists every operation the registration dispatches, sorted.
func OperationNames() []string {
	names := make([]string, 0, len(operations))
	for n := range operations {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// queryErrorCodes are the legacy query codes the SQS model gives error shapes
// whose name differs, as aws-sdk-go v1 records them, and the Coral framework
// errors' query codes as captured from AWS.
var queryErrorCodes = map[string]string{
	awserrors.ErrorSQSQueueDoesNotExist:              "AWS.SimpleQueueService.NonExistentQueue",
	awserrors.ErrorSQSQueueNameExists:                "QueueAlreadyExists",
	awserrors.ErrorSQSQueueDeletedRecently:           "AWS.SimpleQueueService.QueueDeletedRecently",
	awserrors.ErrorSQSMessageNotInflight:             "AWS.SimpleQueueService.MessageNotInflight",
	awserrors.ErrorSQSEmptyBatchRequest:              "AWS.SimpleQueueService.EmptyBatchRequest",
	awserrors.ErrorSQSTooManyEntriesInBatchRequest:   "AWS.SimpleQueueService.TooManyEntriesInBatchRequest",
	awserrors.ErrorSQSBatchEntryIdsNotDistinct:       "AWS.SimpleQueueService.BatchEntryIdsNotDistinct",
	awserrors.ErrorSQSInvalidBatchEntryId:            "AWS.SimpleQueueService.InvalidBatchEntryId",
	awserrors.ErrorSQSBatchRequestTooLong:            "AWS.SimpleQueueService.BatchRequestTooLong",
	awserrors.ErrorUnsupportedOperation:              "AWS.SimpleQueueService.UnsupportedOperation",
	awserrors.ErrorInvalidParameterValueException:    "InvalidParameterValue",
	awserrors.ErrorMissingRequiredParameterException: "MissingParameter",
	awserrors.ErrorSerializationException:            "MalformedInput",
	awserrors.ErrorUnknownOperationException:         "InvalidAction",
	awserrors.ErrorAccessDeniedException:             "AccessDenied",
	awserrors.ErrorServiceUnavailableException:       "ServiceUnavailable",
}

// coralErrors are raised by the AWS framework rather than the SQS model.
var coralErrors = map[string]string{
	awserrors.ErrorInvalidParameterValueException:    envelope.CoralNamespace,
	awserrors.ErrorMissingRequiredParameterException: envelope.CoralNamespace,
	awserrors.ErrorSerializationException:            envelope.CoralNamespace,
	awserrors.ErrorUnknownOperationException:         envelope.CoralNamespace,
	awserrors.ErrorAccessDeniedException:             envelope.CoralNamespace,
	awserrors.ErrorServiceUnavailableException:       envelope.CoralNamespace,
}

// NewRegistration returns the public SQS JSON 1.0 registration.
func NewRegistration(deps Deps) dispatch.Registration {
	return dispatch.Registration{
		Service:         ServiceName,
		ResolveAction:   resolveAction,
		Dispatch:        deps.dispatch,
		Errors:          dispatch.ErrorEnvelopeJSON10Query,
		QueryErrorCodes: queryErrorCodes,
		ErrorNamespace:  ErrorNamespace,
		ErrorNamespaces: coralErrors,
		Inventory:       dispatch.Inventory{Registered: OperationNames()},
	}
}

// resolveAction accepts only "AmazonSQS.<Operation>"; anything else, the
// query protocol included, names no operation.
func resolveAction(r *http.Request) string {
	op, ok := strings.CutPrefix(r.Header.Get("X-Amz-Target"), TargetPrefix)
	if !ok || strings.Contains(op, ".") {
		return ""
	}
	return op
}

func (d Deps) dispatch(w http.ResponseWriter, inv dispatch.Invocation) error {
	r := inv.Request
	h, ok := operations[resolveAction(r)]
	if !ok {
		return errors.New(awserrors.ErrorUnknownOperationException)
	}
	if inv.AccountID == "" || inv.Region == "" {
		slog.ErrorContext(r.Context(), "SQS dispatch: no account or region in the invocation")
		return errors.New(awserrors.ErrorInternalFailure)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, sigv4.MaxPayloadLen+1))
	if err != nil {
		return errors.New(awserrors.ErrorSerializationException)
	}
	if int64(len(body)) > sigv4.MaxPayloadLen {
		return errors.New(awserrors.ErrorRequestEntityTooLarge)
	}
	return h(d, w, inv, body)
}

// decode reads exactly one JSON object into v: null, any other value, or
// anything but whitespace after the object is a SerializationException.
// Unknown members are ignored, provisionally until compared against AWS.
func decode(body []byte, v any) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		trimmed = []byte("{}")
	}
	if trimmed[0] != '{' {
		return errors.New(awserrors.ErrorSerializationException)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(v); err != nil {
		return errors.New(awserrors.ErrorSerializationException)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New(awserrors.ErrorSerializationException)
	}
	return nil
}

func missing(param string) error {
	return awserrors.Errorf(awserrors.ErrorMissingRequiredParameterException, "The request must contain the parameter %s.", param)
}

// authorize reports a denial as SQS's framework AccessDenied. The tag keys
// AWS defines for these actions are not passed: the evaluator does not enforce
// them and fails closed on any statement that names one.
func authorize(inv dispatch.Invocation, action, resource string) error {
	err := inv.Authorize(ServiceName, action, []string{resource}, nil)
	if err == nil {
		return nil
	}
	if code, ok := awserrors.ResolveErrorCode(err); ok && code == awserrors.ErrorAccessDenied {
		return errors.New(awserrors.ErrorAccessDeniedException)
	}
	return err
}

// ownerError maps a queue owner error to its SQS wire code. Codes not
// characterised against AWS yet are provisional.
func ownerError(err error) error {
	switch {
	case errors.Is(err, queue.ErrQueueDoesNotExist):
		return errors.New(awserrors.ErrorSQSQueueDoesNotExist)
	case errors.Is(err, queue.ErrQueueNameExists):
		return errors.New(awserrors.ErrorSQSQueueNameExists)
	case errors.Is(err, queue.ErrQueueDeletedRecently):
		return errors.New(awserrors.ErrorSQSQueueDeletedRecently)
	case errors.Is(err, queue.ErrInvalidAttributeName):
		return awserrors.Errorf(awserrors.ErrorSQSInvalidAttributeName, "%s", err.Error())
	case errors.Is(err, queue.ErrInvalidAttributeValue):
		return awserrors.Errorf(awserrors.ErrorSQSInvalidAttributeValue, "%s", err.Error())
	case errors.Is(err, queue.ErrUnsupportedAttribute):
		return awserrors.Errorf(awserrors.ErrorUnsupportedOperation, "%s", err.Error())
	case errors.Is(err, queue.ErrInvalidQueueName), errors.Is(err, queue.ErrInvalidTag),
		errors.Is(err, queue.ErrInvalidListRequest):
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValueException, "%s", err.Error())
	case errors.Is(err, queue.ErrUnavailable):
		return errors.New(awserrors.ErrorServiceUnavailableException)
	}
	return errors.New(awserrors.ErrorInternalFailure)
}

func writeJSON(w http.ResponseWriter, v any) error {
	out, err := json.Marshal(v)
	if err != nil {
		return errors.New(awserrors.ErrorInternalFailure)
	}
	w.Header().Set("Content-Type", envelope.JSON10ContentType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(out); err != nil {
		slog.Error("SQS: failed to write response", "err", err)
	}
	return nil
}

func queueURL(endpoint, account, name string) string {
	return endpoint + "/" + account + "/" + name
}

func queueARN(inv dispatch.Invocation, name string) string {
	return arn.FormatSQSQueue(inv.Region, inv.AccountID, name)
}
