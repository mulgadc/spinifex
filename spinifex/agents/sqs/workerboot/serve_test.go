package workerboot

import (
	"context"
	"errors"
	"net"
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
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/internal/gwsign"
	"github.com/mulgadc/spinifex/internal/sqsworkergw"
	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerkey"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/relay"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
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

// localStore is the guest's own SQS NATS server: loopback only, no auth,
// JetStream kept under the worker disk so a restart finds its data.
func localStore(t *testing.T, g *guest, port int) *server.Server {
	t.Helper()
	if port == 0 {
		port = -1
	}
	ns, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: port, JetStream: true, StoreDir: g.cfg.DataDir, NoLog: true, NoSigs: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("store server did not start")
	}
	t.Cleanup(func() { ns.Shutdown(); ns.WaitForShutdown() })
	return ns
}

func storePort(ns *server.Server) int { return ns.Addr().(*net.TCPAddr).Port }

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// servingWorker is the guest's ready step serving through the local
// gateway's signed work actions against the local store on port.
type servingWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	logs   []string
	g      *guest
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

func (w *servingWorker) ready() bool {
	_, err := os.Stat(filepath.Join(w.g.cfg.RunDir, readyFile))
	return err == nil
}

func (c gwCluster) startServing(t *testing.T, g *guest, signer *gwsign.Signer, port int) *servingWorker {
	t.Helper()
	cfg := ReadyConfig{Config: g.cfg, Checker: NATSChecker{Port: port, Dial: time.Second}}
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
	cfg.Wait, cfg.Refresh, cfg.Refused, cfg.CallTimeout = 20*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond, 5*time.Second
	ctx, cancel := context.WithCancel(context.Background())
	w := &servingWorker{cancel: cancel, done: make(chan struct{}), g: g}
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

// eventually repeats call until it succeeds.
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

// bootedWorker launches a worker on a new volume and boots its guest, which
// creates and records the receipt key; it returns the guest and its signer.
func (c gwCluster) bootedWorker(t *testing.T) (*guest, *gwsign.Signer, workerident.Intent) {
	t.Helper()
	in := c.launch(t, gwInstance)
	g := newGuest(t)
	signer := c.guestFor(t, g, "ASIAWORKER1", gwInstance)
	if _, err := g.boot(); err != nil {
		t.Fatal(err)
	}
	return g, signer, in
}

// The SDK path through one SQS VM: the worker serves create, send, receive
// and delete from its local store; after the store and worker restart on the
// same disk the queue is still there and an earlier handle still works.
func TestServe_SDKPathThroughTheWorkerSurvivesRestart(t *testing.T) {
	r := &relay.Relay{}
	c := newGWCluster(t, r, nil)
	g, signer, _ := c.bootedWorker(t)
	ns := localStore(t, g, 0)
	port := storePort(ns)
	w := c.startServing(t, g, signer, port)
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
	ns.Shutdown()
	ns.WaitForShutdown()
	g.resetMounts()
	if _, err := g.boot(); err != nil {
		t.Fatalf("reboot: %v", err)
	}
	localStore(t, g, port)
	c.startServing(t, g, signer, port)
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
		if strings.Contains(b, string(key)) {
			t.Fatal("a request carried key material")
		}
	}
}

// The store comes up before the worker serves: while the local store is not
// answering the guest is not ready and no call is served; once it starts,
// readiness and serving follow.
func TestServe_UnavailableStoreIsNeverReady(t *testing.T) {
	r := &relay.Relay{CallLimit: time.Second, PollerStale: 200 * time.Millisecond}
	c := newGWCluster(t, r, nil)
	g, signer, _ := c.bootedWorker(t)
	port := freePort(t)
	w := c.startServing(t, g, signer, port)
	cl := publicSQS(t, r)
	ctx := context.Background()
	eventually(t, func() error {
		if !w.logged("store not ready") {
			return errors.New("no failed check yet")
		}
		return nil
	})
	if w.ready() {
		t.Fatal("ready without a store")
	}
	if _, err := cl.ListQueues(ctx, &sqs.ListQueuesInput{}); err == nil {
		t.Fatal("a call was served without a store")
	}
	localStore(t, g, port)
	eventually(t, func() error {
		_, err := cl.ListQueues(ctx, &sqs.ListQueuesInput{})
		return err
	})
	eventually(t, func() error {
		if !w.ready() {
			return errors.New("not ready")
		}
		return nil
	})
}

// A worker whose recorded receipt key is gone, or replaced, never serves and
// never makes a new key, so handles issued earlier are not invalidated.
func TestServe_ReceiptKeyLossOrChangeNeverServes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, path string)
		want   error
	}{
		{"missing", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, workerkey.ErrReceiptKeyMissing},
		{"changed", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(strings.Repeat("k", workerkey.ReceiptKeyLen)), 0o600); err != nil {
				t.Fatal(err)
			}
		}, workerkey.ErrKeyMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &relay.Relay{CallLimit: time.Second, PollerStale: 200 * time.Millisecond}
			c := newGWCluster(t, r, nil)
			g, signer, _ := c.bootedWorker(t)
			port := storePort(localStore(t, g, 0))
			w := c.startServing(t, g, signer, port)
			cl := publicSQS(t, r)
			eventually(t, func() error {
				_, err := cl.ListQueues(context.Background(), &sqs.ListQueuesInput{})
				return err
			})
			w.stop()

			tc.mutate(t, g.keyFile())
			again := c.startServing(t, g, signer, port)
			eventually(t, func() error {
				if again.logged(tc.want.Error()) {
					return nil
				}
				return errors.New("refusal not logged yet")
			})
			if _, err := cl.ListQueues(context.Background(), &sqs.ListQueuesInput{}); err == nil {
				t.Fatal("a call was served without the recorded receipt key")
			}
			g.resetMounts()
			if _, err := g.boot(); !errors.Is(err, tc.want) {
				t.Fatalf("reboot = %v, want %v", err, tc.want)
			}
			if tc.name == "missing" {
				if _, err := os.Lstat(g.keyFile()); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("a replacement receipt key was created")
				}
			}
		})
	}
}

// A worker the domain has recorded as stopped cannot take work.
func TestServe_StoppedWorkerTakesNoWork(t *testing.T) {
	r := &relay.Relay{CallLimit: time.Second, PollerStale: 200 * time.Millisecond}
	c := newGWCluster(t, r, nil)
	g, signer, in := c.bootedWorker(t)
	w := c.startServing(t, g, signer, storePort(localStore(t, g, 0)))
	cl := publicSQS(t, r)
	ctx := context.Background()
	eventually(t, func() error {
		_, err := cl.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("orders")})
		return err
	})
	if err := c.ids.RecordStopped(ctx, in, gwInstance); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if _, err := cl.ListQueues(ctx, &sqs.ListQueuesInput{}); err == nil {
			return errors.New("still served")
		}
		return nil
	})
	if !w.logged("AccessDenied") {
		t.Fatal("the worker's polls were not refused")
	}
}

// A gateway reporting a multi-node host cluster is refused before the worker
// opens its store, so no queue or marker state is created.
func TestServe_MultiNodeHostCreatesNoStoreState(t *testing.T) {
	c := newGWCluster(t, &relay.Relay{}, func() int { return 3 })
	g, signer, _ := c.bootedWorker(t)
	port := storePort(localStore(t, g, 0))
	w := c.startServing(t, g, signer, port)
	eventually(t, func() error {
		if !w.logged(ErrUnsupportedTopology.Error()) {
			return errors.New("not yet refused")
		}
		return nil
	})
	w.stop()

	js, closeConn, err := NATSConnector{Port: port}.Connect(context.Background())
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
