package workerboot

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/internal/sqsworkergw"
	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerkey"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/relay"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerserve"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
)

const tenantAccount = "123456789012"

// publicSQS serves the public SQS registration over the relay, as the
// ingress would after authenticating the tenant; tenant SigV4 and IAM are
// not under test here.
func publicSQS(t *testing.T, r *relay.Relay) *sqs.Client {
	t.Helper()
	var reg dispatch.Registration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		err := reg.Dispatch(w, dispatch.Invocation{
			Request: req, AccountID: tenantAccount, Region: testRegion, Service: awsapi.ServiceName,
			Authorize: func(string, string, []string, iampolicy.ConditionKeys) error { return nil },
			Actor: func() (dispatch.Actor, error) {
				return dispatch.Actor{UserID: "AIDAPOLLER", AccountID: tenantAccount}, nil
			},
		})
		if err != nil {
			code, msg, _ := awserrors.ResolveErrorDetail(err)
			if code == "" {
				code = err.Error()
			}
			meta := awserrors.LookupErrorMessage(awsapi.ServiceName, code)
			if msg == "" {
				msg = meta.Message
			}
			qc := code
			if q, ok := reg.QueryErrorCodes[code]; ok {
				qc = q
			}
			envelope.WriteJSON(w, envelope.ProtocolJSON10Query, envelope.JSONError{
				Code: code, Verbatim: true, Message: msg, Status: meta.HTTPCode, Namespace: reg.ErrorNamespace, QueryCode: qc,
			})
		}
	}))
	t.Cleanup(srv.Close)
	reg = awsapi.NewRegistration(awsapi.Deps{
		Queues: r, Messages: r, Receives: r, Deletes: r, Visibility: r, Endpoint: srv.URL,
	})
	return sqs.New(sqs.Options{
		BaseEndpoint: aws.String(srv.URL), Region: testRegion, RetryMaxAttempts: 1,
		Credentials: aws.AnonymousCredentials{},
	})
}

// servingWorker is the worker's ready step serving through the local
// gateway's signed work actions against a real store on port.
type servingWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	logs   []string
}

func (w *servingWorker) log(msg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.logs = append(w.logs, msg)
}

func (w *servingWorker) logged(substr string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.ContainsFunc(w.logs, func(l string) bool { return strings.Contains(l, substr) })
}

func (c gwCluster) startServing(t *testing.T, g *guest, accessKey, inst string, port int) *servingWorker {
	t.Helper()
	cfg := c.readyFor(g, accessKey, inst, port)
	signer := c.gw.issue(accessKey, "secret-"+accessKey, inst)
	cfg.Serve = &ServeConfig{
		Connector: NATSConnector{Port: port},
		Run: func(ctx context.Context, region, url string, js jetstream.JetStream, receiptKey []byte) error {
			gw, err := sqsworkergw.New(url, region, c.gw.caPEM(), signer)
			if err != nil {
				return err
			}
			return workerserve.Run(ctx, gw, js, receiptKey, cfg.Log)
		},
	}
	cfg.Wait, cfg.Refresh, cfg.Refused = 20*time.Millisecond, time.Second, 50*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	w := &servingWorker{cancel: cancel, done: make(chan struct{})}
	cfg.Log = w.log
	go func() {
		defer close(w.done)
		_ = Watch(ctx, cfg)
	}()
	t.Cleanup(w.stop)
	return w
}

func (w *servingWorker) stop() {
	w.once.Do(w.cancel)
	<-w.done
}

// eventually repeats call until it succeeds, for the worker's first poll.
func eventually(t *testing.T, call func() error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := call()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never succeeded: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// bootServingWorker boots a bound worker, starts a store admitting only its
// key, binds the store and returns the store's port.
func (c gwCluster) bootServingWorker(t *testing.T, g *guest) int {
	t.Helper()
	pub, err := g.boot()
	if err != nil {
		t.Fatal(err)
	}
	port := storeServer(t, pub)
	c.bindStore(t, pub, "127.0.0.1")
	return port
}

// The SDK path through a real worker: create, send, receive and delete run
// in the worker against the dedicated store; after a worker restart the
// queue is still there and a handle issued before the restart still works.
func TestServe_SDKPathThroughTheWorkerSurvivesRestart(t *testing.T) {
	r := &relay.Relay{}
	c := newGWClusterWith(t, r)
	c.launch(t, "claim-0001", 0, gwInstance)
	g := newGuest(t)
	c.guestFor(t, g, "ASIAWORKER1", gwInstance)
	port := c.bootServingWorker(t, g)
	w := c.startServing(t, g, "ASIAWORKER1", gwInstance, port)
	cl := publicSQS(t, r)
	ctx := context.Background()

	var url string
	eventually(t, func() error {
		out, err := cl.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("orders")})
		if err == nil {
			url = aws.ToString(out.QueueUrl)
		}
		return err
	})
	body := strings.Repeat(`"quoted" \ body `, 4096)
	sent, err := cl.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl: aws.String(url), MessageBody: aws.String(body),
		MessageAttributes: map[string]sqstypes.MessageAttributeValue{
			"kind": {DataType: aws.String("String"), StringValue: aws.String("new\norder")},
			"blob": {DataType: aws.String("Binary"), BinaryValue: []byte{0, 1, 2, 255}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := cl.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: aws.String(url), MaxNumberOfMessages: 10, WaitTimeSeconds: 2,
		MessageAttributeNames: []string{"All"}, VisibilityTimeout: 60,
	})
	if err != nil || len(got.Messages) != 1 {
		t.Fatalf("receive = %v, %v", got, err)
	}
	m := got.Messages[0]
	if aws.ToString(m.MessageId) != aws.ToString(sent.MessageId) || aws.ToString(m.Body) != body ||
		aws.ToString(m.MD5OfMessageAttributes) != aws.ToString(sent.MD5OfMessageAttributes) ||
		aws.ToString(m.MessageAttributes["kind"].StringValue) != "new\norder" {
		t.Fatalf("received a different message: %+v", m)
	}
	if _, err := cl.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(url), ReceiptHandle: m.ReceiptHandle}); err != nil {
		t.Fatal(err)
	}

	if _, err := cl.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(url), MessageBody: aws.String("second")}); err != nil {
		t.Fatal(err)
	}
	held, err := cl.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(url), WaitTimeSeconds: 2, VisibilityTimeout: 60})
	if err != nil || len(held.Messages) != 1 {
		t.Fatalf("receive before restart = %v, %v", held, err)
	}

	w.stop()
	g.resetMounts()
	if _, err := g.boot(); err != nil {
		t.Fatal(err)
	}
	c.startServing(t, g, "ASIAWORKER1", gwInstance, port)
	eventually(t, func() error {
		_, err := cl.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("orders")})
		return err
	})
	if _, err := cl.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(url), ReceiptHandle: held.Messages[0].ReceiptHandle}); err != nil {
		t.Fatalf("handle from before the restart: %v", err)
	}
	empty, err := cl.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(url)})
	if err != nil || len(empty.Messages) != 0 {
		t.Fatalf("queue after delete = %v, %v", empty, err)
	}

	key, err := workerkey.Store{Dir: g.keyDir()}.ReceiptKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range c.gw.sentBodies() {
		if strings.Contains(b, string(key)) || strings.Contains(b, strings.TrimSpace(g.seed())) {
			t.Fatal("a request carried key material")
		}
	}
}

// A worker whose receipt key is gone, or replaced, never serves: it does not
// make a new key, so handles issued earlier are not silently invalidated.
func TestServe_ReceiptKeyLossOrChangeNeverServes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, path string)
		want   error
	}{
		{"missing", func(t *testing.T, path string) { mustRemove(t, path) }, workerkey.ErrReceiptKeyMissing},
		{"changed", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(strings.Repeat("k", workerkey.ReceiptKeyLen)), 0o600); err != nil {
				t.Fatal(err)
			}
		}, workerserve.ErrReceiptKeyChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &relay.Relay{CallLimit: time.Second, PollerStale: 200 * time.Millisecond}
			c := newGWClusterWith(t, r)
			c.launch(t, "claim-0001", 0, gwInstance)
			g := newGuest(t)
			c.guestFor(t, g, "ASIAWORKER1", gwInstance)
			port := c.bootServingWorker(t, g)
			w := c.startServing(t, g, "ASIAWORKER1", gwInstance, port)
			cl := publicSQS(t, r)
			eventually(t, func() error {
				_, err := cl.ListQueues(context.Background(), &sqs.ListQueuesInput{})
				return err
			})
			w.stop()

			path := filepath.Join(g.keyDir(), "receipt.key")
			tc.mutate(t, path)
			again := c.startServing(t, g, "ASIAWORKER1", gwInstance, port)
			eventually(t, func() error {
				if again.logged(tc.want.Error()) {
					return nil
				}
				return errors.New("refusal not logged yet")
			})
			if _, err := cl.ListQueues(context.Background(), &sqs.ListQueuesInput{}); err == nil {
				t.Fatal("a call was served without the original receipt key")
			}
			if tc.name == "missing" {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("a replacement receipt key was created")
				}
			}
		})
	}
}

// A worker the domain has recorded as stopped cannot take work, so calls
// fail as unavailable rather than reaching it.
func TestServe_StoppedWorkerTakesNoWork(t *testing.T) {
	r := &relay.Relay{CallLimit: time.Second, PollerStale: 200 * time.Millisecond}
	c := newGWClusterWith(t, r)
	first := c.launch(t, "claim-0001", 0, gwInstance)
	g := newGuest(t)
	c.guestFor(t, g, "ASIAWORKER1", gwInstance)
	port := c.bootServingWorker(t, g)
	w := c.startServing(t, g, "ASIAWORKER1", gwInstance, port)
	cl := publicSQS(t, r)
	ctx := context.Background()
	eventually(t, func() error {
		_, err := cl.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("orders")})
		return err
	})
	if err := c.ids.RecordStopped(ctx, first, gwInstance); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		_, err := cl.ListQueues(ctx, &sqs.ListQueuesInput{})
		if err == nil {
			return errors.New("still served")
		}
		return nil
	})
	if !w.logged("AccessDenied") {
		t.Fatal("the worker's polls were not refused")
	}
}

// A gateway that reports a multi-node host cluster is refused before the
// worker connects to its store, so no queue or marker state is created.
func TestServe_MultiNodeHostCreatesNoStoreState(t *testing.T) {
	r := &relay.Relay{}
	c := newGWClusterHosts(t, r, func() int { return 3 })
	c.launch(t, "claim-0001", 0, gwInstance)
	g := newGuest(t)
	c.guestFor(t, g, "ASIAWORKER1", gwInstance)
	pub, err := g.boot()
	if err != nil {
		t.Fatal(err)
	}
	port := storeServer(t, pub)
	c.bindStore(t, pub, "127.0.0.1")

	w := c.startServing(t, g, "ASIAWORKER1", gwInstance, port)
	eventually(t, func() error {
		if !w.logged(ErrUnsupportedTopology.Error()) {
			return errors.New("not yet refused")
		}
		return nil
	})
	w.stop()

	kp, err := workerkey.Store{Dir: g.keyDir()}.ForExistingStore(pub)
	if err != nil {
		t.Fatal(err)
	}
	defer kp.Wipe()
	js, closeConn, err := NATSConnector{Port: port}.Connect(context.Background(), "127.0.0.1", testStorePin, kp)
	if err != nil {
		t.Fatal(err)
	}
	defer closeConn()
	names := js.StreamNames(context.Background())
	var got []string
	for n := range names.Name() {
		got = append(got, n)
	}
	if names.Err() != nil || len(got) != 0 {
		t.Fatalf("store streams = %v, %v; want none", got, names.Err())
	}
}
