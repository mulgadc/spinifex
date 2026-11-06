//go:build lambdatestlink

package awsgw

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/aws/aws-sdk-go/service/sqs/sqsiface"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/eventsource"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// Poller timing. The receive is a long poll, so an idle queue costs one
// request per esmWaitSeconds; a session is reissued well before it expires.
const (
	esmWaitSeconds    = 20
	esmReconcileEvery = 5 * time.Second
	esmMaxBackoff     = 60 * time.Second
	esmSessionSeconds = 900
	esmRenewBefore    = 10 * time.Minute
	esmDeleteTimeout  = 15 * time.Second
	// esmMaxEvent is the synchronous request bound one event must fit.
	esmMaxEvent = 6 * 1024 * 1024
	// esmMaxReason bounds a recorded failure reason.
	esmMaxReason = 512
)

// esmInvoker runs one event through Invoke's checks, without Invoke's IAM check.
type esmInvoker interface {
	InvokeEventSource(ctx context.Context, accountID, region, name string, payload []byte) (awsapi.SyncResult, error)
}

var _ esmInvoker = awsapi.Deps{}

// esmStore is what the poller needs of the mapping store.
type esmStore interface {
	Get(ctx context.Context, uuid string) (eventsource.Mapping, uint64, error)
	List(ctx context.Context) ([]eventsource.Entry, error)
	Update(ctx context.Context, m eventsource.Mapping, revision uint64) (uint64, error)
	Remove(ctx context.Context, uuid string, revision uint64) error
}

var _ esmStore = (*eventsource.Store)(nil)

// esmRuntime polls every enabled SQS mapping on this gateway. It is one loop
// per gateway with no cross-node lease, so it is correct on a single node only.
type esmRuntime struct {
	store    func(ctx context.Context) (esmStore, error)
	sessions lambdaEnvironmentRoleIssuer
	region   string
	meta     function.Client
	invoke   esmInvoker
	// queue builds an SQS client signed as one session; nil means the
	// gateway handler, set by start.
	queue func(cred *credentials.Credentials) sqsiface.SQSAPI
	now   func() time.Time
	// interval and wait pace reconcile and back-off; zero uses the defaults.
	interval time.Duration
	backoff  time.Duration

	mu      sync.Mutex
	handler http.Handler
	// endpoint is the public SQS base URL queue URLs are built on.
	endpoint string
}

func newESMRuntime(open func(ctx context.Context) (*eventsource.Store, error), sessions lambdaEnvironmentRoleIssuer, region string) *esmRuntime {
	store := func(ctx context.Context) (esmStore, error) { return open(ctx) }
	return &esmRuntime{store: store, sessions: sessions, region: region, now: time.Now}
}

func (r *esmRuntime) sources() awsapi.SourceChecker { return r }

// start runs the poller until ctx ends; deps is the composed Lambda control plane.
func (r *esmRuntime) start(ctx context.Context, h http.Handler, deps awsapi.Deps, sqsEndpoint string) {
	r.mu.Lock()
	r.handler = h
	r.endpoint = sqsEndpoint
	if r.queue == nil {
		r.queue = r.handlerQueue
	}
	r.meta = deps.Meta
	r.invoke = deps
	r.mu.Unlock()
	go r.run(ctx)
}

// handlerTransport carries a signed SDK request straight into the gateway
// handler. It never leaves the TLS-terminating gateway, so it is marked as
// received over TLS and is authorized exactly as one from a client.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	in := req.Clone(req.Context())
	in.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true}
	in.RemoteAddr = "127.0.0.1:0"
	in.RequestURI = req.URL.RequestURI()
	if in.Host == "" {
		in.Host = req.URL.Host
	}
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, in)
	return rec.Result(), nil
}

func (r *esmRuntime) handlerQueue(cred *credentials.Credentials) sqsiface.SQSAPI {
	r.mu.Lock()
	h, endpoint := r.handler, r.endpoint
	r.mu.Unlock()
	sess := session.Must(session.NewSession(&aws.Config{
		Region:      aws.String(r.region),
		Endpoint:    aws.String(endpoint),
		Credentials: cred,
		HTTPClient:  &http.Client{Transport: handlerTransport{h: h}},
		MaxRetries:  aws.Int(0),
	}))
	return sqs.New(sess)
}

// esmSession is one execution-role session the poller signs with.
type esmSession struct {
	cred    *credentials.Credentials
	keyID   string
	role    string
	expires time.Time
	scope   stsdomain.LambdaEnvironmentSessionScope
}

func (r *esmRuntime) issue(ctx context.Context, rec function.Record) (*esmSession, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	scope := stsdomain.LambdaEnvironmentSessionScope{
		AccountID:     rec.AccountID,
		FunctionID:    rec.FunctionID,
		FunctionARN:   arn.FormatLambdaFunction(rec.Region, rec.AccountID, rec.FunctionName),
		EnvironmentID: "esm-" + hex.EncodeToString(b),
	}
	out, err := r.sessions.AssumeRoleForLambdaEnvironment(ctx, stsdomain.LambdaEnvironmentSessionRequest{
		AccountID: scope.AccountID, RoleARN: rec.Role, FunctionARN: scope.FunctionARN,
		FunctionID: scope.FunctionID, EnvironmentID: scope.EnvironmentID, MaxLifetimeSeconds: esmSessionSeconds,
	})
	if err != nil {
		return nil, err
	}
	c := out.Credentials
	return &esmSession{
		cred: credentials.NewStaticCredentials(aws.StringValue(c.AccessKeyId), aws.StringValue(c.SecretAccessKey),
			aws.StringValue(c.SessionToken)),
		keyID: aws.StringValue(c.AccessKeyId), role: rec.Role, expires: aws.TimeValue(c.Expiration), scope: scope,
	}, nil
}

func (r *esmRuntime) revoke(s *esmSession) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), revokeTimeout)
	defer cancel()
	if err := r.sessions.RevokeLambdaEnvironmentSession(ctx, s.keyID, s.scope); err != nil {
		slog.Warn("Lambda event source: poller session not revoked; it stays valid until expiry",
			"environment_id", s.scope.EnvironmentID, "err", err)
	}
}

// fresh returns s while it is the function's role and outlives need;
// otherwise it issues a replacement and revokes s, which no request holds.
func (r *esmRuntime) fresh(ctx context.Context, rec function.Record, s *esmSession, need time.Duration) (*esmSession, error) {
	if s != nil && s.role == rec.Role && s.expires.Sub(r.now()) > need {
		return s, nil
	}
	next, err := r.issue(ctx, rec)
	if err != nil {
		return s, err
	}
	r.revoke(s)
	return next, nil
}

// eventClient renews *s before each event unless it outlives that event's
// invocation and delete, so no acknowledgement runs on expired credentials.
func (r *esmRuntime) eventClient(ctx context.Context, rec function.Record, s **esmSession) func() (sqsiface.SQSAPI, error) {
	need := time.Duration(rec.Timeout)*time.Second + esmDeleteTimeout + time.Minute
	return func() (sqsiface.SQSAPI, error) {
		next, err := r.fresh(ctx, rec, *s, need)
		*s = next
		if err != nil {
			return nil, err
		}
		return r.queue(next.cred), nil
	}
}

func queueName(queueARN string) (account, name string) {
	parts := strings.Split(queueARN, ":")
	if len(parts) != 6 {
		return "", ""
	}
	return parts[4], parts[5]
}

// queueURL builds the queue's URL rather than asking GetQueueUrl, which the
// execution role is not required to allow.
func (r *esmRuntime) queueURL(queueARN string) (string, error) {
	r.mu.Lock()
	endpoint := r.endpoint
	r.mu.Unlock()
	account, name := queueName(queueARN)
	if endpoint == "" || name == "" {
		return "", errors.New("SQS is not served on this gateway")
	}
	return endpoint + "/" + account + "/" + name, nil
}

// QueueVisibilityTimeout reads the queue as the function's execution role
// with a short-lived session it revokes before returning.
func (r *esmRuntime) QueueVisibilityTimeout(ctx context.Context, rec function.Record, queueARN string) (int64, error) {
	s, err := r.issue(ctx, rec)
	if err != nil {
		if code, ok := awserrors.ResolveErrorCode(err); ok && code == awserrors.ErrorAccessDenied {
			return 0, awserrors.Errorf(awserrors.ErrorInvalidParameterValueException, "The role defined for the function cannot be assumed by Lambda.")
		}
		return 0, err
	}
	defer r.revoke(s)
	q := r.queue(s.cred)
	url, err := r.queueURL(queueARN)
	if err != nil {
		return 0, awserrors.Errorf(awserrors.ErrorServiceException, "%s", err.Error())
	}
	out, err := q.GetQueueAttributesWithContext(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(url), AttributeNames: aws.StringSlice([]string{sqs.QueueAttributeNameVisibilityTimeout}),
	})
	if err != nil {
		return 0, sourceError(err)
	}
	var v int64
	if _, err := fmt.Sscan(aws.StringValue(out.Attributes[sqs.QueueAttributeNameVisibilityTimeout]), &v); err != nil {
		return 0, fmt.Errorf("queue visibility timeout: %w", err)
	}
	return v, nil
}

// sourceError reports a queue the execution role cannot read as the
// caller's fault, as Lambda does.
func sourceError(err error) error {
	var ae awserr.Error
	if !errors.As(err, &ae) {
		return err
	}
	switch ae.Code() {
	case "AccessDenied", "AccessDeniedException":
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValueException,
			"The function execution role does not have permissions to call GetQueueAttributes on SQS")
	case sqs.ErrCodeQueueDoesNotExist, "QueueDoesNotExist":
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValueException,
			"Error occurred while ReceiveMessage. SQS Error Code: AWS.SimpleQueueService.NonExistentQueue. SQS Error Message: The specified queue does not exist.")
	}
	return err
}

type esmWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (w *esmWorker) stop() {
	w.cancel()
	<-w.done
}

func (r *esmRuntime) run(ctx context.Context) {
	workers := map[string]*esmWorker{}
	defer func() {
		for _, w := range workers {
			w.stop()
		}
	}()
	every := r.interval
	if every == 0 {
		every = esmReconcileEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		r.reconcile(ctx, workers)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// reconcile starts a worker for each enabled mapping, including those
// written before a restart, and finishes each delete only after its worker
// has stopped, so no invoke or message delete follows it.
func (r *esmRuntime) reconcile(ctx context.Context, workers map[string]*esmWorker) {
	store, err := r.store(ctx)
	if err != nil {
		slog.Warn("Lambda event source: mapping store unavailable", "err", err)
		return
	}
	entries, err := store.List(ctx)
	if err != nil {
		slog.Warn("Lambda event source: list mappings failed", "err", err)
		return
	}
	seen := map[string]bool{}
	for _, e := range entries {
		m := e.Mapping
		seen[m.UUID] = true
		switch m.State {
		case eventsource.StateEnabled:
			if workers[m.UUID] == nil {
				wctx, cancel := context.WithCancel(ctx)
				w := &esmWorker{cancel: cancel, done: make(chan struct{})}
				workers[m.UUID] = w
				go func() {
					defer close(w.done)
					r.work(wctx, m)
				}()
			}
		case eventsource.StateDeleting:
			if w := workers[m.UUID]; w != nil {
				w.stop()
				delete(workers, m.UUID)
			}
			if err := r.finishDelete(ctx, store, m.UUID); err != nil {
				slog.Warn("Lambda event source: mapping not removed; retrying", "uuid", m.UUID, "err", err)
			}
		}
	}
	for id, w := range workers {
		if !seen[id] {
			w.stop()
			delete(workers, id)
		}
	}
}

func (r *esmRuntime) finishDelete(ctx context.Context, store esmStore, id string) error {
	m, rev, err := store.Get(ctx, id)
	if errors.Is(err, eventsource.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if m.State != eventsource.StateDeleting {
		return nil
	}
	return store.Remove(ctx, id, rev)
}

// setReason records a receive failure on an enabled mapping, or clears it
// back to USER_INITIATED with reason "".
func (r *esmRuntime) setReason(ctx context.Context, id, reason string) {
	if reason == "" {
		reason = eventsource.ReasonUserInitiated
	} else {
		reason = "PROBLEM: " + reason
		if len(reason) > esmMaxReason {
			reason = reason[:esmMaxReason]
		}
	}
	store, err := r.store(ctx)
	if err != nil {
		return
	}
	m, rev, err := store.Get(ctx, id)
	if err != nil || m.State != eventsource.StateEnabled || m.StateTransitionReason == reason {
		return
	}
	m.StateTransitionReason = reason
	if _, err := store.Update(ctx, m, rev); err != nil && !errors.Is(err, eventsource.ErrConflict) {
		slog.Warn("Lambda event source: reason not recorded", "uuid", id, "err", err)
	}
}

func (r *esmRuntime) wait(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// work polls one mapping until ctx ends. A failure keeps the mapping, records
// why, and retries after a back-off.
func (r *esmRuntime) work(ctx context.Context, m eventsource.Mapping) {
	var s *esmSession
	defer func() { r.revoke(s) }()
	base := r.backoff
	if base == 0 {
		base = time.Second
	}
	backoff := base
	fail := func(reason string) {
		r.setReason(context.WithoutCancel(ctx), m.UUID, reason)
		r.wait(ctx, backoff)
		backoff = min(2*backoff, esmMaxBackoff)
	}
	for ctx.Err() == nil {
		rec, _, err := r.meta.Get(ctx, m.AccountID, m.Region, m.FunctionName)
		if err != nil {
			fail("Function lookup failed: " + err.Error())
			continue
		}
		next, err := r.fresh(ctx, rec, s, esmRenewBefore)
		s = next
		if err != nil {
			fail("The execution role could not be assumed: " + err.Error())
			continue
		}
		q := r.queue(s.cred)
		url, err := r.queueURL(m.EventSourceARN)
		if err != nil {
			fail("Error occurred while ReceiveMessage: " + err.Error())
			continue
		}
		out, err := q.ReceiveMessageWithContext(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:              aws.String(url),
			MaxNumberOfMessages:   aws.Int64(m.BatchSize),
			WaitTimeSeconds:       aws.Int64(esmWaitSeconds),
			AttributeNames:        aws.StringSlice([]string{"All"}),
			MessageAttributeNames: aws.StringSlice([]string{"All"}),
		})
		if err != nil {
			if ctx.Err() == nil {
				fail("Error occurred while ReceiveMessage: " + err.Error())
			}
			continue
		}
		r.setReason(ctx, m.UUID, "")
		if err := r.process(ctx, m, r.eventClient(ctx, rec, &s), url, out.Messages); err != nil {
			slog.Warn("Lambda event source: batch not processed; messages stay on the queue", "uuid", m.UUID, "err", err)
			r.wait(ctx, backoff)
			backoff = min(2*backoff, esmMaxBackoff)
			continue
		}
		backoff = base
	}
}

// sqsRecord and sqsAttribute are the AWS SQS event record shape.
type sqsRecord struct {
	MessageID         string                  `json:"messageId"`
	ReceiptHandle     string                  `json:"receiptHandle"`
	Body              string                  `json:"body"`
	Attributes        map[string]string       `json:"attributes"`
	MessageAttributes map[string]sqsAttribute `json:"messageAttributes"`
	MD5OfBody         string                  `json:"md5OfBody"`
	EventSource       string                  `json:"eventSource"`
	EventSourceARN    string                  `json:"eventSourceARN"`
	AWSRegion         string                  `json:"awsRegion"`
}

type sqsAttribute struct {
	StringValue      *string  `json:"stringValue,omitempty"`
	BinaryValue      []byte   `json:"binaryValue,omitempty"`
	StringListValues []string `json:"stringListValues"`
	BinaryListValues [][]byte `json:"binaryListValues"`
	DataType         string   `json:"dataType"`
}

func eventRecord(m eventsource.Mapping, msg *sqs.Message) sqsRecord {
	rec := sqsRecord{
		MessageID: aws.StringValue(msg.MessageId), ReceiptHandle: aws.StringValue(msg.ReceiptHandle),
		Body: aws.StringValue(msg.Body), Attributes: map[string]string{}, MessageAttributes: map[string]sqsAttribute{},
		MD5OfBody: aws.StringValue(msg.MD5OfBody), EventSource: "aws:sqs", EventSourceARN: m.EventSourceARN, AWSRegion: m.Region,
	}
	for k, v := range msg.Attributes {
		rec.Attributes[k] = aws.StringValue(v)
	}
	for k, v := range msg.MessageAttributes {
		rec.MessageAttributes[k] = sqsAttribute{
			StringValue: v.StringValue, BinaryValue: v.BinaryValue, StringListValues: []string{},
			BinaryListValues: [][]byte{}, DataType: aws.StringValue(v.DataType),
		}
	}
	return rec
}

// esmEvent is one invocation's payload and the messages it carries.
type esmEvent struct {
	Payload  []byte
	Messages []*sqs.Message
}

// splitEvents groups messages into events whose encoding fits esmMaxEvent.
// A message too large for an event of its own is returned in oversized and
// is neither invoked nor deleted.
func splitEvents(m eventsource.Mapping, msgs []*sqs.Message) (events []esmEvent, oversized []*sqs.Message) {
	const frame = len(`{"Records":[]}`)
	var recs []json.RawMessage
	var batch []*sqs.Message
	size := frame
	flush := func() {
		if len(recs) == 0 {
			return
		}
		payload, _ := json.Marshal(struct {
			Records []json.RawMessage `json:"Records"`
		}{recs})
		events = append(events, esmEvent{Payload: payload, Messages: batch})
		recs, batch, size = nil, nil, frame
	}
	for _, msg := range msgs {
		enc, err := json.Marshal(eventRecord(m, msg))
		if err != nil || frame+len(enc) > esmMaxEvent {
			oversized = append(oversized, msg)
			continue
		}
		extra := len(enc)
		if len(recs) > 0 {
			extra++
		}
		if size+extra > esmMaxEvent {
			flush()
			extra = len(enc)
		}
		recs = append(recs, enc)
		batch = append(batch, msg)
		size += extra
	}
	flush()
	return events, oversized
}

// process invokes each event and deletes its messages only on a classified
// success with no function error; anything else leaves them to reappear.
// process invokes and acknowledges each event in turn. It returns the first
// failure, so the worker backs off before receiving again.
func (r *esmRuntime) process(ctx context.Context, m eventsource.Mapping, client func() (sqsiface.SQSAPI, error), url string, msgs []*sqs.Message) error {
	events, oversized := splitEvents(m, msgs)
	if len(oversized) > 0 {
		r.setReason(ctx, m.UUID, fmt.Sprintf("%d message(s) exceed the %d-byte event limit and are not delivered", len(oversized), esmMaxEvent))
	}
	for _, ev := range events {
		// A stopped worker invokes nothing further; delete waits for it.
		if ctx.Err() != nil {
			return nil
		}
		q, err := client()
		if err != nil {
			return fmt.Errorf("execution role session: %w", err)
		}
		res, err := r.invoke.InvokeEventSource(ctx, m.AccountID, m.Region, m.FunctionName, ev.Payload)
		if err != nil {
			return err
		}
		if res.FunctionError != "" {
			return fmt.Errorf("function error %s", res.FunctionError)
		}
		entries := make([]*sqs.DeleteMessageBatchRequestEntry, len(ev.Messages))
		for i, msg := range ev.Messages {
			entries[i] = &sqs.DeleteMessageBatchRequestEntry{Id: aws.String(fmt.Sprint(i)), ReceiptHandle: msg.ReceiptHandle}
		}
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), esmDeleteTimeout)
		out, err := q.DeleteMessageBatchWithContext(dctx, &sqs.DeleteMessageBatchInput{QueueUrl: aws.String(url), Entries: entries})
		cancel()
		if err != nil {
			slog.Warn("Lambda event source: processed messages not deleted; they will be delivered again", "uuid", m.UUID, "err", err)
		} else if len(out.Failed) > 0 {
			slog.Warn("Lambda event source: some processed messages not deleted", "uuid", m.UUID, "failed", len(out.Failed))
		}
	}
	return nil
}
