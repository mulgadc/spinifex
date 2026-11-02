//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	awsv1 "github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/smithy-go"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	sqsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi"
	sqsworkerapi "github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi/worker"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/relay"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerserve"
	awsdispatch "github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// relayWorker hands relay work to the worker loop in process, with the bounds
// the worker's PollWork and CompleteWork actions apply; their signed channel
// is covered by the worker's own tests.
type relayWorker struct {
	r     *relay.Relay
	taker relay.Taker
}

var _ workerserve.Gateway = relayWorker{}

func (w relayWorker) PollWork(ctx context.Context) ([]relay.Work, error) {
	pollCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return w.r.Take(pollCtx, w.taker, 16, 16<<20), nil
}

func (w relayWorker) CompleteWork(_ context.Context, id string, result []byte) (string, error) {
	var res relay.Result
	if err := json.Unmarshal(result, &res); err != nil {
		return "", err
	}
	outcome, err := w.r.Complete(w.taker, id, res)
	if err != nil {
		return "", err
	}
	return sqsworkerapi.OutcomeName(outcome), nil
}

// startSQSWorker runs the real worker loop and owners against its own
// dedicated store, never the gateway's platform NATS.
func startSQSWorker(t *testing.T, r *relay.Relay) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		taker := relay.Taker{InstanceID: "i-0123456789abcdef0", Generation: 1, ClaimID: "claim-0001"}
		_ = workerserve.Run(ctx, relayWorker{r: r, taker: taker}, js, key, nil)
	}()
	t.Cleanup(func() { cancel(); <-done })
}

func sqsClient(gw *Gateway, accessKey, secret string) *sqs.Client {
	return sqs.New(sqs.Options{
		BaseEndpoint: aws.String(gw.Server.URL), Region: testRegion, RetryMaxAttempts: 1,
		Credentials: credentials.NewStaticCredentialsProvider(accessKey, secret, ""),
	})
}

func errorCode(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}

// platformTraffic records every message published in the test's platform
// NATS account, to show SQS payloads never cross it.
type platformTraffic struct {
	mu   sync.Mutex
	data [][]byte
}

func watchPlatform(t *testing.T, nc *nats.Conn) *platformTraffic {
	t.Helper()
	p := &platformTraffic{}
	sub, err := nc.Subscribe(">", func(m *nats.Msg) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.data = append(p.data, append(append([]byte{}, m.Subject...), m.Data...))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return p
}

func (p *platformTraffic) seen() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.data)
}

func (p *platformTraffic) carried(marker string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.data {
		if bytes.Contains(d, []byte(marker)) {
			return true
		}
	}
	return false
}

// Real SigV4 and IAM at the gateway, then the relay and a real worker loop
// on its dedicated store: create, send, receive and delete. Denied and
// unsigned calls stop at the gateway, and no queue name or body reaches
// platform NATS.
func TestSQS_SignedTenantRequestsReachTheWorker(t *testing.T) {
	r := &relay.Relay{CallLimit: 5 * time.Second}
	gw := StartGatewayWithServices(t, []awsdispatch.Registration{sqsawsapi.NewRegistration(sqsawsapi.Deps{
		Queues: r, Messages: r, Receives: r, Deletes: r, Visibility: r, Endpoint: "https://localhost:9999",
	})})
	traffic := watchPlatform(t, gw.NATSConn)
	ctx := context.Background()
	root := sqsClient(gw, testAccessKeyID, testSecretAccessKey)

	// No worker is polling yet, so the gateway refuses rather than queueing.
	_, err := root.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("early")})
	require.Error(t, err)
	require.Equal(t, "ServiceUnavailable", errorCode(err))

	startSQSWorker(t, r)
	const queueName, body = "orders-x7q", `payload-marker-3f9a "quoted" \ body`
	var url string
	require.Eventually(t, func() bool {
		out, err := root.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(queueName)})
		if err == nil {
			url = aws.ToString(out.QueueUrl)
		}
		return err == nil
	}, 10*time.Second, 20*time.Millisecond)
	require.True(t, strings.HasSuffix(url, "/"+gw.AccountID+"/"+queueName), url)

	sent, err := root.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(url), MessageBody: aws.String(body)})
	require.NoError(t, err)
	got, err := root.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: aws.String(url), MaxNumberOfMessages: 10, WaitTimeSeconds: 1, VisibilityTimeout: 60,
	})
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	require.Equal(t, aws.ToString(sent.MessageId), aws.ToString(got.Messages[0].MessageId))
	require.Equal(t, body, aws.ToString(got.Messages[0].Body))
	_, err = root.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(url), ReceiptHandle: got.Messages[0].ReceiptHandle})
	require.NoError(t, err)

	// An IAM user allowed only SendMessage is denied ReceiveMessage by policy.
	iamCli := gw.IAMClient(t)
	_, err = iamCli.CreateUser(&iam.CreateUserInput{UserName: awsv1.String("sqs-sender")})
	require.NoError(t, err)
	_, err = iamCli.PutUserPolicy(&iam.PutUserPolicyInput{
		UserName: awsv1.String("sqs-sender"), PolicyName: awsv1.String("send-only"),
		PolicyDocument: awsv1.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"sqs:SendMessage","Resource":"*"}]}`),
	})
	require.NoError(t, err)
	keyOut, err := iamCli.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: awsv1.String("sqs-sender")})
	require.NoError(t, err)
	sender := sqsClient(gw, awsv1.StringValue(keyOut.AccessKey.AccessKeyId), awsv1.StringValue(keyOut.AccessKey.SecretAccessKey))
	require.Eventually(t, func() bool {
		_, err := sender.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(url), MessageBody: aws.String("from-sender")})
		return err == nil
	}, 5*time.Second, 50*time.Millisecond)
	_, err = sender.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(url)})
	require.Equal(t, "AccessDenied", errorCode(err))

	// A wrong secret fails SigV4 before any work is queued.
	_, err = sqsClient(gw, testAccessKeyID, "not-the-secret").ListQueues(ctx, &sqs.ListQueuesInput{})
	require.Equal(t, "SignatureDoesNotMatch", errorCode(err))

	require.NotZero(t, traffic.seen(), "the platform watcher saw no traffic at all")
	require.False(t, traffic.carried("payload-marker-3f9a"), "a message body crossed platform NATS")
	require.False(t, traffic.carried(queueName), "a queue name crossed platform NATS")
}
