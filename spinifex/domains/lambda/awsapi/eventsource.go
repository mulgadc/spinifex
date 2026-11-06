package awsapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/eventsource"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// MappingStore is what the handlers need of the event-source mapping store.
type MappingStore interface {
	Create(ctx context.Context, m eventsource.Mapping) error
	Get(ctx context.Context, uuid string) (eventsource.Mapping, uint64, error)
	List(ctx context.Context) ([]eventsource.Entry, error)
	Update(ctx context.Context, m eventsource.Mapping, revision uint64) (uint64, error)
}

var _ MappingStore = (*eventsource.Store)(nil)

// SourceChecker reads an SQS queue's visibility timeout as the function's
// execution role, so a queue the role cannot read is refused at create.
type SourceChecker interface {
	QueueVisibilityTimeout(ctx context.Context, rec function.Record, queueARN string) (int64, error)
}

// Batch size bounds for an SQS standard queue without a batching window.
const (
	defaultBatchSize = 10
	maxBatchSize     = 10
)

// mappingCreateFields are the CreateEventSourceMapping members this build
// accepts; every other member is refused rather than ignored.
var mappingCreateFields = map[string]bool{
	"FunctionName": true, "EventSourceArn": true, "BatchSize": true, "Enabled": true,
	"MaximumBatchingWindowInSeconds": true,
}

var mappingUUIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func mappingNotFound() error {
	return awserrors.Errorf(awserrors.ErrorResourceNotFoundException, "The resource you requested does not exist.")
}

// mappingARN is the IAM resource of one mapping, or "*" for a malformed UUID.
func mappingARN(region, accountID, uuid string) string {
	if region == "" || accountID == "" || !mappingUUIDRE.MatchString(uuid) {
		return "*"
	}
	return fmt.Sprintf("arn:aws:lambda:%s:%s:event-source-mapping:%s", region, accountID, uuid)
}

type mappingCreate struct {
	FunctionName, QueueARN string
	BatchSize              int64
}

// decodeMappingCreate reads the body strictly and refuses every option this
// build does not implement.
func decodeMappingCreate(body []byte) (*mappingCreate, error) {
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&raw); err != nil {
		return nil, invalid("The request body could not be parsed as JSON.")
	}
	for k := range raw {
		if !mappingCreateFields[k] {
			return nil, unsupported(k)
		}
	}
	var in struct {
		FunctionName                   *string `json:"FunctionName"`
		EventSourceArn                 *string `json:"EventSourceArn"`
		BatchSize                      *int64  `json:"BatchSize"`
		Enabled                        *bool   `json:"Enabled"`
		MaximumBatchingWindowInSeconds *int64  `json:"MaximumBatchingWindowInSeconds"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, invalid("The request body could not be parsed as JSON.")
	}
	if in.FunctionName == nil || *in.FunctionName == "" {
		return nil, invalid("1 validation error detected: Value null at 'functionName' failed to satisfy constraint: Member must not be null")
	}
	if in.EventSourceArn == nil || *in.EventSourceArn == "" {
		return nil, invalid("Unrecognized event source, must be kinesis, dynamodb stream or sqs.")
	}
	if in.Enabled != nil && !*in.Enabled {
		return nil, unsupported("Enabled false")
	}
	if in.MaximumBatchingWindowInSeconds != nil && *in.MaximumBatchingWindowInSeconds != 0 {
		return nil, unsupported("MaximumBatchingWindowInSeconds")
	}
	m := &mappingCreate{FunctionName: *in.FunctionName, QueueARN: *in.EventSourceArn, BatchSize: defaultBatchSize}
	if in.BatchSize != nil {
		if *in.BatchSize < 1 || *in.BatchSize > maxBatchSize {
			return nil, invalid("BatchSize must be between 1 and %d for an SQS queue without a batching window.", maxBatchSize)
		}
		m.BatchSize = *in.BatchSize
	}
	return m, nil
}

// checkQueueARN refuses anything but a standard SQS queue in the caller's
// own account and Region.
func checkQueueARN(region, accountID, queueARN string) error {
	parts := strings.Split(queueARN, ":")
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" {
		return invalid("Unrecognized event source, must be kinesis, dynamodb stream or sqs.")
	}
	if parts[2] != "sqs" {
		return unsupported("an event source other than SQS")
	}
	if parts[3] != region || parts[4] != accountID {
		return unsupported("a queue in another account or Region")
	}
	if parts[5] == "" {
		return invalid("Unrecognized event source, must be kinesis, dynamodb stream or sqs.")
	}
	if strings.HasSuffix(parts[5], ".fifo") {
		return unsupported("a FIFO queue")
	}
	return nil
}

func newMappingUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func mappingShape(m eventsource.Mapping, state string) *lambda.EventSourceMappingConfiguration {
	return &lambda.EventSourceMappingConfiguration{
		UUID:                           aws.String(m.UUID),
		BatchSize:                      aws.Int64(m.BatchSize),
		MaximumBatchingWindowInSeconds: aws.Int64(0),
		EventSourceArn:                 aws.String(m.EventSourceARN),
		FunctionArn:                    aws.String(m.FunctionARN),
		LastModified:                   aws.Time(m.LastModified),
		State:                          aws.String(state),
		StateTransitionReason:          aws.String(m.StateTransitionReason),
	}
}

func (d Deps) mappings(ctx context.Context) (MappingStore, error) {
	if d.Mappings == nil {
		return nil, serviceError("event-source mappings are unavailable")
	}
	return d.Mappings(ctx)
}

// createEventSourceMapping resolves the function, reads the queue as its
// execution role, and records an Enabled mapping its poller picks up.
func (d Deps) createEventSourceMapping(ctx context.Context, c *call) (reply, error) {
	in, err := decodeMappingCreate(c.Body)
	if err != nil {
		return reply{}, err
	}
	if err := checkQueueARN(c.Region, c.AccountID, in.QueueARN); err != nil {
		return reply{}, err
	}
	name, q, err := resolveFunctionName(c.Region, c.AccountID, in.FunctionName)
	if err != nil {
		return reply{}, err
	}
	if q != "" {
		return reply{}, unsupported("a qualified FunctionName")
	}
	rec, _, err := d.Meta.Get(ctx, c.AccountID, c.Region, name)
	if errors.Is(err, function.ErrNotFound) {
		return reply{}, notFound(c.Region, c.AccountID, name)
	}
	if err != nil {
		return reply{}, err
	}
	if d.Sources == nil {
		return reply{}, serviceError("event-source mappings are unavailable")
	}
	visibility, err := d.Sources.QueueVisibilityTimeout(ctx, rec, in.QueueARN)
	if err != nil {
		return reply{}, err
	}
	if rec.Timeout > visibility {
		return reply{}, invalid("Queue visibility timeout: %d seconds is less than Function timeout: %d seconds", visibility, rec.Timeout)
	}
	store, err := d.mappings(ctx)
	if err != nil {
		return reply{}, err
	}
	entries, err := store.List(ctx)
	if err != nil {
		return reply{}, err
	}
	for _, e := range entries {
		m := e.Mapping
		if m.AccountID == c.AccountID && m.EventSourceARN == in.QueueARN && m.FunctionName == name {
			return reply{}, awserrors.Errorf(awserrors.ErrorResourceConflictException,
				"An event source mapping with SQS arn (\" %s \") and function (\" %s \") already exists. Please update or delete the existing mapping with UUID %s",
				in.QueueARN, name, m.UUID)
		}
	}
	uuid, err := newMappingUUID()
	if err != nil {
		return reply{}, err
	}
	m := eventsource.Mapping{
		FormatVersion: eventsource.FormatVersion, UUID: uuid, AccountID: c.AccountID, Region: c.Region,
		FunctionName: name, FunctionARN: arn.FormatLambdaFunction(c.Region, c.AccountID, name),
		EventSourceARN: in.QueueARN, BatchSize: in.BatchSize, State: eventsource.StateEnabled,
		StateTransitionReason: eventsource.ReasonUserInitiated, LastModified: time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := store.Create(ctx, m); err != nil {
		return reply{}, err
	}
	return reply{Status: http.StatusAccepted, Body: mappingShape(m, "Creating")}, nil
}

// ownMapping reads a mapping of the caller's account and Region.
func (d Deps) ownMapping(ctx context.Context, c *call) (MappingStore, eventsource.Mapping, uint64, error) {
	if !mappingUUIDRE.MatchString(c.Params[0]) {
		return nil, eventsource.Mapping{}, 0, mappingNotFound()
	}
	store, err := d.mappings(ctx)
	if err != nil {
		return nil, eventsource.Mapping{}, 0, err
	}
	m, rev, err := store.Get(ctx, c.Params[0])
	if errors.Is(err, eventsource.ErrNotFound) || (err == nil && (m.AccountID != c.AccountID || m.Region != c.Region)) {
		return nil, eventsource.Mapping{}, 0, mappingNotFound()
	}
	return store, m, rev, err
}

func (d Deps) getEventSourceMapping(ctx context.Context, c *call) (reply, error) {
	_, m, _, err := d.ownMapping(ctx, c)
	if err != nil {
		return reply{}, err
	}
	return reply{Status: http.StatusOK, Body: mappingShape(m, m.State)}, nil
}

// deleteEventSourceMapping records Deleting and returns; the poller stops its
// worker and only then removes the record.
func (d Deps) deleteEventSourceMapping(ctx context.Context, c *call) (reply, error) {
	for range 3 {
		store, m, rev, err := d.ownMapping(ctx, c)
		if err != nil {
			return reply{}, err
		}
		if m.State == eventsource.StateDeleting {
			return reply{Status: http.StatusAccepted, Body: mappingShape(m, m.State)}, nil
		}
		m.State = eventsource.StateDeleting
		m.StateTransitionReason = eventsource.ReasonUserInitiated
		m.LastModified = time.Now().UTC().Truncate(time.Millisecond)
		_, err = store.Update(ctx, m, rev)
		if errors.Is(err, eventsource.ErrConflict) {
			continue
		}
		if err != nil {
			return reply{}, err
		}
		return reply{Status: http.StatusAccepted, Body: mappingShape(m, m.State)}, nil
	}
	return reply{}, serviceError("the mapping changed concurrently; retry the request")
}

func (d Deps) listEventSourceMappings(ctx context.Context, c *call) (reply, error) {
	for _, k := range []string{"Marker"} {
		if _, ok := c.Query[k]; ok {
			return reply{}, unsupported(k)
		}
	}
	maxItems := 100
	if v := c.Query.Get("MaxItems"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListItems {
			return reply{}, invalid("MaxItems must be between 1 and %d", maxListItems)
		}
		maxItems = n
	}
	fnName := ""
	if v := c.Query.Get("FunctionName"); v != "" {
		name, _, err := resolveFunctionName(c.Region, c.AccountID, v)
		if err != nil {
			return reply{}, err
		}
		fnName = name
	}
	source := c.Query.Get("EventSourceArn")
	store, err := d.mappings(ctx)
	if err != nil {
		return reply{}, err
	}
	entries, err := store.List(ctx)
	if err != nil {
		return reply{}, err
	}
	out := []*lambda.EventSourceMappingConfiguration{}
	for _, e := range entries {
		m := e.Mapping
		if m.AccountID != c.AccountID || m.Region != c.Region ||
			(fnName != "" && m.FunctionName != fnName) || (source != "" && m.EventSourceARN != source) {
			continue
		}
		if len(out) == maxItems {
			break
		}
		out = append(out, mappingShape(m, m.State))
	}
	return reply{Status: http.StatusOK, Body: &lambda.ListEventSourceMappingsOutput{EventSourceMappings: out}}, nil
}
