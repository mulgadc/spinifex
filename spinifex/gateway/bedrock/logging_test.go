package gateway_bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubLoggingConfigReader returns a fixed (cfg, ok, err) for every Get call.
type stubLoggingConfigReader struct {
	cfg LoggingConfig
	ok  bool
	err error
}

func (s stubLoggingConfigReader) Get(context.Context, string) (LoggingConfig, bool, error) {
	return s.cfg, s.ok, s.err
}

func TestLoggingConfigStore_PutGetDelete(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	store := NewLoggingConfigStore(js, 1)
	ctx := context.Background()

	_, ok, err := store.Get(ctx, "000000000001")
	require.NoError(t, err)
	assert.False(t, ok)

	cfg := LoggingConfig{S3BucketName: "acct-bucket", S3KeyPrefix: "bedrock", TextDataDeliveryEnabled: true}
	require.NoError(t, store.Put(ctx, "000000000001", cfg))

	got, ok, err := store.Get(ctx, "000000000001")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, cfg, got)

	require.NoError(t, store.Delete(ctx, "000000000001"))
	_, ok, err = store.Get(ctx, "000000000001")
	require.NoError(t, err)
	assert.False(t, ok)

	// Deleting an already-absent config is idempotent, not an error.
	require.NoError(t, store.Delete(ctx, "000000000001"))
}

func TestPutModelInvocationLoggingConfiguration_RejectsCloudWatch(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	store := NewLoggingConfigStore(js, 1)

	input := &bedrock.PutModelInvocationLoggingConfigurationInput{
		LoggingConfig: &bedrock.LoggingConfig{
			CloudWatchConfig:        &bedrock.CloudWatchConfig{LogGroupName: aws.String("g"), RoleArn: aws.String("arn:aws:iam::000000000001:role/r")},
			TextDataDeliveryEnabled: aws.Bool(true),
		},
	}
	_, err := PutModelInvocationLoggingConfiguration(context.Background(), "000000000001", store, input)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, err.Error())
}

func TestPutModelInvocationLoggingConfiguration_RequiresBucketWhenDeliveryEnabled(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	store := NewLoggingConfigStore(js, 1)

	input := &bedrock.PutModelInvocationLoggingConfigurationInput{
		LoggingConfig: &bedrock.LoggingConfig{TextDataDeliveryEnabled: aws.Bool(true)},
	}
	_, err := PutModelInvocationLoggingConfiguration(context.Background(), "000000000001", store, input)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, err.Error())
}

// TestPutModelInvocationLoggingConfiguration_RequiresBucketForEmbeddingDelivery
// covers embedding delivery on its own: enabling it with no bucket is as
// undeliverable as text or image, so it must not slip past the same gate.
func TestPutModelInvocationLoggingConfiguration_RequiresBucketForEmbeddingDelivery(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	store := NewLoggingConfigStore(js, 1)

	input := &bedrock.PutModelInvocationLoggingConfigurationInput{
		LoggingConfig: &bedrock.LoggingConfig{EmbeddingDataDeliveryEnabled: aws.Bool(true)},
	}
	_, err := PutModelInvocationLoggingConfiguration(context.Background(), "000000000001", store, input)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, err.Error())
}

// TestModelInvocationLoggingConfiguration_RoundTripsViaSDKStructs drives the
// three control-plane handlers with real aws-sdk-go v1 bedrock structs, the
// same shapes a genuine SDK client marshals, rather than this package's own
// internal LoggingConfig.
func TestModelInvocationLoggingConfiguration_RoundTripsViaSDKStructs(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	store := NewLoggingConfigStore(js, 1)
	ctx := context.Background()

	putInput := &bedrock.PutModelInvocationLoggingConfigurationInput{
		LoggingConfig: &bedrock.LoggingConfig{
			S3Config:                     &bedrock.S3Config{BucketName: aws.String("acct-bucket"), KeyPrefix: aws.String("bedrock")},
			TextDataDeliveryEnabled:      aws.Bool(true),
			ImageDataDeliveryEnabled:     aws.Bool(false),
			EmbeddingDataDeliveryEnabled: aws.Bool(true),
		},
	}
	_, err := PutModelInvocationLoggingConfiguration(ctx, "000000000001", store, putInput)
	require.NoError(t, err)

	getOut, err := GetModelInvocationLoggingConfiguration(ctx, "000000000001", store, new(bedrock.GetModelInvocationLoggingConfigurationInput))
	require.NoError(t, err)
	require.NotNil(t, getOut.LoggingConfig)
	require.NotNil(t, getOut.LoggingConfig.S3Config)
	assert.Equal(t, "acct-bucket", aws.StringValue(getOut.LoggingConfig.S3Config.BucketName))
	assert.Equal(t, "bedrock", aws.StringValue(getOut.LoggingConfig.S3Config.KeyPrefix))
	assert.True(t, aws.BoolValue(getOut.LoggingConfig.TextDataDeliveryEnabled))
	assert.False(t, aws.BoolValue(getOut.LoggingConfig.ImageDataDeliveryEnabled))
	// A flag the internal LoggingConfig has no field for is accepted by Put and
	// then silently absent from Get, so assert the round-trip, not just the Put.
	assert.True(t, aws.BoolValue(getOut.LoggingConfig.EmbeddingDataDeliveryEnabled))

	_, err = DeleteModelInvocationLoggingConfiguration(ctx, "000000000001", store, new(bedrock.DeleteModelInvocationLoggingConfigurationInput))
	require.NoError(t, err)

	getOut, err = GetModelInvocationLoggingConfiguration(ctx, "000000000001", store, new(bedrock.GetModelInvocationLoggingConfigurationInput))
	require.NoError(t, err)
	assert.Nil(t, getOut.LoggingConfig)
}

// TestStreamRecorder_DropsAndCountsWhenStreamMissing proves the "publish
// failure must never fail the invocation" contract: Record has no error
// return at all, so the only way to observe a failed publish is Dropped().
func TestStreamRecorder_DropsAndCountsWhenStreamMissing(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	// Deliberately skip EnsureInvocationStream: publishing to a subject no
	// stream captures must fail cleanly (no responders), not panic or block.
	recorder := NewStreamRecorder(js, nil)

	recorder.Record(context.Background(), InvocationRecord{RequestID: "req-1", AccountID: "000000000001"})

	assert.Equal(t, int64(1), recorder.Dropped())
}

// TestStreamRecorder_Record_BodyOnlyWhenLoggingEnabled is the D11 gate at the
// publisher: body text only survives onto the stream when the account's own
// logging config enables delivery.
func TestStreamRecorder_Record_BodyOnlyWhenLoggingEnabled(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	_, err := EnsureInvocationStream(ctx, js, 1)
	require.NoError(t, err)
	consumer, err := EnsureDeliveryConsumer(ctx, js)
	require.NoError(t, err)

	enabled := NewStreamRecorder(js, stubLoggingConfigReader{cfg: LoggingConfig{TextDataDeliveryEnabled: true}, ok: true})
	enabled.Record(ctx, InvocationRecord{RequestID: "req-enabled", AccountID: "acct-a", InputText: "prompt", OutputText: "completion"})

	disabled := NewStreamRecorder(js, stubLoggingConfigReader{})
	disabled.Record(ctx, InvocationRecord{RequestID: "req-disabled", AccountID: "acct-b", InputText: "prompt", OutputText: "completion"})

	batch, err := consumer.Fetch(2, jetstream.FetchMaxWait(2*time.Second))
	require.NoError(t, err)
	byID := map[string]InvocationRecord{}
	for msg := range batch.Messages() {
		var r InvocationRecord
		require.NoError(t, json.Unmarshal(msg.Data(), &r))
		byID[r.RequestID] = r
		require.NoError(t, msg.Ack())
	}
	require.NoError(t, batch.Error())
	require.Len(t, byID, 2)

	assert.Equal(t, "prompt", byID["req-enabled"].InputText)
	assert.Equal(t, "completion", byID["req-enabled"].OutputText)
	assert.Empty(t, byID["req-disabled"].InputText)
	assert.Empty(t, byID["req-disabled"].OutputText)
}

// TestDeliveryConsumer_Run_WritesS3AndNeverLogsBodyText is the D11 invariant
// at the delivery side: the account's own body text reaches only its S3
// bucket; the metadata log line — this package's stand-in for "ES delivery"
// (see DeliveryConsumer's doc comment) — must never contain it, even when a
// record legitimately carries a populated body.
func TestDeliveryConsumer_Run_WritesS3AndNeverLogsBodyText(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := EnsureInvocationStream(ctx, js, 1)
	require.NoError(t, err)
	consumer, err := EnsureDeliveryConsumer(ctx, js)
	require.NoError(t, err)

	store := objectstore.NewMemoryObjectStore()
	configs := stubLoggingConfigReader{cfg: LoggingConfig{S3BucketName: "acct-bucket", TextDataDeliveryEnabled: true}, ok: true}

	const secretPrompt = "the secret prompt text"
	const secretCompletion = "the secret completion text"

	recorder := NewStreamRecorder(js, configs)
	recorder.Record(ctx, InvocationRecord{
		RequestID: "req-body-test", AccountID: "acct-a", ModelID: "meta.llama3-70b-instruct-v1:0",
		Operation: OperationConverse, InputText: secretPrompt, OutputText: secretCompletion,
	})

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	dc := NewDeliveryConsumer(store, configs)
	done := make(chan struct{})
	go func() {
		dc.Run(ctx, consumer)
		close(done)
	}()

	require.Eventually(t, func() bool { return store.Count() == 1 }, 2*time.Second, 10*time.Millisecond)
	cancel()
	<-done

	logOutput := logBuf.String()
	assert.NotContains(t, logOutput, secretPrompt)
	assert.NotContains(t, logOutput, secretCompletion)
	assert.Contains(t, logOutput, "req-body-test")
}

// TestDeliveryConsumer_Run_DeliversMetadataOnlyRecordToS3 covers the common
// case: an account configured a destination bucket but left body delivery
// disabled. It must still receive its invocation record in S3 (metadata
// only, InputText/OutputText empty) — S3 delivery is not itself gated on
// body presence, only the body fields are.
func TestDeliveryConsumer_Run_DeliversMetadataOnlyRecordToS3(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := EnsureInvocationStream(ctx, js, 1)
	require.NoError(t, err)
	consumer, err := EnsureDeliveryConsumer(ctx, js)
	require.NoError(t, err)

	store := objectstore.NewMemoryObjectStore()
	// TextDataDeliveryEnabled is deliberately false: the account asked for a
	// destination bucket but not body delivery.
	configs := stubLoggingConfigReader{cfg: LoggingConfig{S3BucketName: "acct-bucket"}, ok: true}

	recorder := NewStreamRecorder(js, configs)
	recorder.Record(ctx, InvocationRecord{
		RequestID: "req-metadata-only", AccountID: "acct-a", ModelID: "meta.llama3-70b-instruct-v1:0",
		Operation: OperationConverse, InputText: "prompt", OutputText: "completion",
	})

	dc := NewDeliveryConsumer(store, configs)
	done := make(chan struct{})
	go func() {
		dc.Run(ctx, consumer)
		close(done)
	}()

	require.Eventually(t, func() bool { return store.Count() == 1 }, 2*time.Second, 10*time.Millisecond)
	cancel()
	<-done

	listing, err := store.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{Bucket: aws.String("acct-bucket")})
	require.NoError(t, err)
	require.Len(t, listing.Contents, 1, "expected the record to land in the account's bucket despite body delivery being disabled")

	obj, err := store.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String("acct-bucket"), Key: listing.Contents[0].Key})
	require.NoError(t, err)
	body, err := io.ReadAll(obj.Body)
	require.NoError(t, err)

	var stored InvocationRecord
	require.NoError(t, json.Unmarshal(body, &stored))
	assert.Equal(t, "req-metadata-only", stored.RequestID)
	assert.Empty(t, stored.InputText)
	assert.Empty(t, stored.OutputText)
}
