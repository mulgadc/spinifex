package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/config"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	handlers_ec2_snapshot "github.com/mulgadc/spinifex/spinifex/handlers/ec2/snapshot"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// selfHostModelID and providerModelID are real catalog entries: the former
// is self-host (weights staging applies), the latter is provider-served
// (weights staging must refuse it), so tests exercise LookupServingSpec's
// found/selfHost distinction against the real catalog rather than a stub.
const (
	selfHostModelID = "meta.llama3-2-1b-instruct-v1:0"
	providerModelID = "anthropic.claude-3-5-sonnet-20240620-v1:0"
)

// newWeightsStoreForTest builds a WeightsStore backed by an embedded
// JetStream server, so stage/list/remove tests exercise the real KV
// idempotency and persistence logic instead of a hand-rolled fake.
func newWeightsStoreForTest(t *testing.T) *gateway_bedrock.WeightsStore {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	return gateway_bedrock.NewWeightsStore(testutil.NewJetStream(t, nc), 1)
}

// explodingObjectStore fails the test immediately if any of its methods is
// called. It stands in for the object store on paths that must do zero S3
// work, such as the idempotent no-op re-stage and the two catalog-refusal
// cases.
type explodingObjectStore struct {
	t *testing.T
}

func (e explodingObjectStore) GetObject(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	e.t.Fatal("GetObject called on a path that must not touch S3")
	return nil, nil
}

func (e explodingObjectStore) HeadObject(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
	e.t.Fatal("HeadObject called on a path that must not touch S3")
	return nil, nil
}

func (e explodingObjectStore) PutObject(context.Context, *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	e.t.Fatal("PutObject called on a path that must not touch S3")
	return nil, nil
}

func (e explodingObjectStore) DeleteObject(context.Context, *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	e.t.Fatal("DeleteObject called on a path that must not touch S3")
	return nil, nil
}

func (e explodingObjectStore) ListObjectsV2(context.Context, *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	e.t.Fatal("ListObjectsV2 called on a path that must not touch S3")
	return nil, nil
}

func (e explodingObjectStore) EnsureBucket(context.Context, string) error {
	e.t.Fatal("EnsureBucket called on a path that must not touch S3")
	return nil
}

var _ objectstore.ObjectStore = explodingObjectStore{}

// explodingMaterializer fails the test immediately if invoked, standing in
// for weightsMaterializer on paths that must refuse before materialising
// anything.
func explodingMaterializer(t *testing.T) weightsMaterializer {
	t.Helper()
	return func(context.Context, string, int64) (string, error) {
		t.Fatal("materialize called on a path that must refuse before materialising")
		return "", nil
	}
}

// explodingSnapshotChecker fails the test immediately if invoked, standing
// in for weightsSnapshotChecker on paths that must never need to consult
// snapshot liveness: a different source URI, or refusal before the
// idempotency branch is even reached.
func explodingSnapshotChecker(t *testing.T) weightsSnapshotChecker {
	t.Helper()
	return func(context.Context, string) (bool, error) {
		t.Fatal("checkSnapshotLive called on a path that must not need it")
		return false, nil
	}
}

// fixedSnapshotChecker always reports live, unconditionally.
func fixedSnapshotChecker(live bool) weightsSnapshotChecker {
	return func(context.Context, string) (bool, error) { return live, nil }
}

func TestParseWeightsS3URI_ValidWithTrailingSlash(t *testing.T) {
	bucket, prefix, err := parseWeightsS3URI("s3://models/llama-3.2-1b/")
	require.NoError(t, err)
	assert.Equal(t, "models", bucket)
	assert.Equal(t, "llama-3.2-1b/", prefix)
}

// TestParseWeightsS3URI_AddsMissingTrailingSlash covers scoping: without the
// trailing slash, downstream prefix listing would also match a sibling
// object whose key merely shares the prefix as a substring.
func TestParseWeightsS3URI_AddsMissingTrailingSlash(t *testing.T) {
	_, prefix, err := parseWeightsS3URI("s3://models/llama-3.2-1b")
	require.NoError(t, err)
	assert.Equal(t, "llama-3.2-1b/", prefix)
}

func TestParseWeightsS3URI_BucketOnlyNoPrefix(t *testing.T) {
	bucket, prefix, err := parseWeightsS3URI("s3://models")
	require.NoError(t, err)
	assert.Equal(t, "models", bucket)
	assert.Empty(t, prefix)
}

func TestParseWeightsS3URI_MissingSchemeIsError(t *testing.T) {
	_, _, err := parseWeightsS3URI("models/llama-3.2-1b/")
	assert.Error(t, err)
}

func TestParseWeightsS3URI_MissingBucketIsError(t *testing.T) {
	_, _, err := parseWeightsS3URI("s3:///llama-3.2-1b/")
	assert.Error(t, err)
}

// putObject seeds a memory object store with key -> body, as if it were an
// already-uploaded Hugging Face model file.
func putObject(t *testing.T, store *objectstore.MemoryObjectStore, bucket, key string, body []byte) {
	t.Helper()
	_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	})
	require.NoError(t, err)
}

func seedCompleteWeightsPrefix(t *testing.T, store *objectstore.MemoryObjectStore, bucket, prefix string) {
	t.Helper()
	putObject(t, store, bucket, prefix+"model.safetensors", nil)
	putObject(t, store, bucket, prefix+"tokenizer.json", nil)
	for _, name := range requiredWeightsFiles {
		putObject(t, store, bucket, prefix+name, nil)
	}
}

// TestValidateWeightsPrefix_AllFilesPresent covers the happy path: every
// required file and at least one *.safetensors file present passes.
func TestValidateWeightsPrefix_AllFilesPresent(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	seedCompleteWeightsPrefix(t, store, "models", "llama-3.2-1b/")

	err := validateWeightsPrefix(context.Background(), store, "models", "llama-3.2-1b/")
	assert.NoError(t, err)
}

// TestValidateWeightsPrefix_MissingRequiredFile covers refusal before any
// materialisation: a typo'd or incomplete prefix must fail validation, not
// be discovered after downloading gigabytes.
func TestValidateWeightsPrefix_MissingRequiredFile(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	putObject(t, store, "models", "llama-3.2-1b/model.safetensors", nil)
	putObject(t, store, "models", "llama-3.2-1b/config.json", nil)
	putObject(t, store, "models", "llama-3.2-1b/tokenizer.json", nil)
	// tokenizer_config.json deliberately omitted.

	err := validateWeightsPrefix(context.Background(), store, "models", "llama-3.2-1b/")
	require.Error(t, err)
	assert.ErrorContains(t, err, "tokenizer_config.json")
}

// TestValidateWeightsPrefix_TokenizerJSONOnlyPasses covers modern
// fast-tokenizer repos such as Llama 3.x, which ship only tokenizer.json at
// the prefix root (tokenizer.model, if present at all, lives under
// original/ and is not required).
func TestValidateWeightsPrefix_TokenizerJSONOnlyPasses(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	putObject(t, store, "models", "llama-3.2-1b/model.safetensors", nil)
	putObject(t, store, "models", "llama-3.2-1b/config.json", nil)
	putObject(t, store, "models", "llama-3.2-1b/tokenizer_config.json", nil)
	putObject(t, store, "models", "llama-3.2-1b/tokenizer.json", nil)

	err := validateWeightsPrefix(context.Background(), store, "models", "llama-3.2-1b/")
	assert.NoError(t, err)
}

// TestValidateWeightsPrefix_TokenizerModelOnlyPasses covers older
// SentencePiece repos, which ship only tokenizer.model with no
// tokenizer.json at all.
func TestValidateWeightsPrefix_TokenizerModelOnlyPasses(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	putObject(t, store, "models", "llama-3.2-1b/model.safetensors", nil)
	putObject(t, store, "models", "llama-3.2-1b/config.json", nil)
	putObject(t, store, "models", "llama-3.2-1b/tokenizer_config.json", nil)
	putObject(t, store, "models", "llama-3.2-1b/tokenizer.model", nil)

	err := validateWeightsPrefix(context.Background(), store, "models", "llama-3.2-1b/")
	assert.NoError(t, err)
}

// TestValidateWeightsPrefix_NeitherTokenizerFilePresent covers refusal when
// neither tokenizer shape is present: the error must name the requirement.
func TestValidateWeightsPrefix_NeitherTokenizerFilePresent(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	putObject(t, store, "models", "llama-3.2-1b/model.safetensors", nil)
	putObject(t, store, "models", "llama-3.2-1b/config.json", nil)
	putObject(t, store, "models", "llama-3.2-1b/tokenizer_config.json", nil)

	err := validateWeightsPrefix(context.Background(), store, "models", "llama-3.2-1b/")
	require.Error(t, err)
	assert.ErrorContains(t, err, "tokenizer.json")
	assert.ErrorContains(t, err, "tokenizer.model")
}

// TestValidateWeightsPrefix_MissingSafetensors covers the variable-name
// weights file: all fixed-name files present is not enough without at least
// one *.safetensors object.
func TestValidateWeightsPrefix_MissingSafetensors(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	putObject(t, store, "models", "llama-3.2-1b/tokenizer.json", nil)
	for _, name := range requiredWeightsFiles {
		putObject(t, store, "models", "llama-3.2-1b/"+name, nil)
	}

	err := validateWeightsPrefix(context.Background(), store, "models", "llama-3.2-1b/")
	require.Error(t, err)
	assert.ErrorContains(t, err, "*.safetensors")
}

// TestValidateWeightsPrefix_EmptyPrefixIsAllMissing covers a wrong/typo'd
// prefix that resolves to nothing: every required file (and *.safetensors)
// is reported missing, rather than a bare "not found".
func TestValidateWeightsPrefix_EmptyPrefixIsAllMissing(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()

	err := validateWeightsPrefix(context.Background(), store, "models", "does-not-exist/")
	require.Error(t, err)
	for _, name := range requiredWeightsFiles {
		assert.ErrorContains(t, err, name)
	}
	assert.ErrorContains(t, err, "*.safetensors")
	assert.ErrorContains(t, err, "tokenizer.json")
	assert.ErrorContains(t, err, "tokenizer.model")
}

// TestDownloadWeightsPrefix_FlattensToBasename covers the download step:
// predastore's key structure is flattened to the file's basename in destDir,
// since buildWeightsImage only needs a flat directory of files.
func TestDownloadWeightsPrefix_FlattensToBasename(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	content := []byte("weights-bytes")
	putObject(t, store, "models", "llama-3.2-1b/model.safetensors", content)
	putObject(t, store, "models", "llama-3.2-1b/config.json", []byte("{}"))

	destDir := t.TempDir()
	total, err := downloadWeightsPrefix(context.Background(), store, "models", "llama-3.2-1b/", destDir)
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)+len("{}")), total)

	got, err := os.ReadFile(filepath.Join(destDir, "model.safetensors"))
	require.NoError(t, err)
	assert.Equal(t, content, got)
}

// TestRunStageWeights_IdempotentReStageIsNoop covers the no-op path: staging
// the same --s3-uri a second time, with the recorded snapshot still live,
// must not touch the object store or materialize anything at all, and must
// report the existing snapshot.
func TestRunStageWeights_IdempotentReStageIsNoop(t *testing.T) {
	weightsStore := newWeightsStoreForTest(t)
	ctx := context.Background()
	require.NoError(t, weightsStore.PutWeights(ctx, selfHostModelID, "s3://models/llama-3.2-1b/", "snap-0001"))

	msg, err := runStageWeights(ctx, explodingObjectStore{t: t}, weightsStore, t.TempDir(),
		selfHostModelID, "s3://models/llama-3.2-1b/", explodingMaterializer(t), fixedSnapshotChecker(true))

	require.NoError(t, err)
	assert.Contains(t, msg, "already staged")
	assert.Contains(t, msg, "snap-0001")

	// The record must be untouched: still the same snapshot.
	entry, ok, err := weightsStore.GetWeights(ctx, selfHostModelID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "snap-0001", entry.SnapshotID)
}

// TestRunStageWeights_SelfHealsWhenSnapshotGone covers the same --s3-uri
// re-stage when the recorded snapshot no longer exists (host rebuild, volume
// GC): stage must not short-circuit on the stale KV record. It must
// re-materialise, write the new snapshot, and name the missing snapshot it
// replaced -- distinctly from the different-source-URI replace wording --
// so an operator never has to run 'weights remove' first.
func TestRunStageWeights_SelfHealsWhenSnapshotGone(t *testing.T) {
	weightsStore := newWeightsStoreForTest(t)
	ctx := context.Background()
	require.NoError(t, weightsStore.PutWeights(ctx, selfHostModelID, "s3://models/llama-3.2-1b/", "snap-missing"))

	store := objectstore.NewMemoryObjectStore()
	seedCompleteWeightsPrefix(t, store, "models", "llama-3.2-1b/")

	materializeCalled := false
	materialize := func(context.Context, string, int64) (string, error) {
		materializeCalled = true
		return "snap-healed", nil
	}

	msg, err := runStageWeights(ctx, store, weightsStore, t.TempDir(),
		selfHostModelID, "s3://models/llama-3.2-1b/", materialize, fixedSnapshotChecker(false))

	require.NoError(t, err)
	assert.True(t, materializeCalled, "self-heal must re-materialize rather than short-circuit")
	assert.Contains(t, msg, "MISSING")
	assert.Contains(t, msg, "snap-missing")
	assert.Contains(t, msg, "snap-healed")

	entry, ok, err := weightsStore.GetWeights(ctx, selfHostModelID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "snap-healed", entry.SnapshotID, "PutWeights must record the new snapshot")
}

// TestRunStageWeights_LiveCheckErrorSurfaces covers the checker's own error
// path: a real liveness-check failure (as opposed to a definitive not-found)
// must be returned rather than guessed at either way.
func TestRunStageWeights_LiveCheckErrorSurfaces(t *testing.T) {
	weightsStore := newWeightsStoreForTest(t)
	ctx := context.Background()
	require.NoError(t, weightsStore.PutWeights(ctx, selfHostModelID, "s3://models/llama-3.2-1b/", "snap-0001"))

	checkErr := errors.New("predastore unreachable")
	checker := func(context.Context, string) (bool, error) { return false, checkErr }

	_, err := runStageWeights(ctx, explodingObjectStore{t: t}, weightsStore, t.TempDir(),
		selfHostModelID, "s3://models/llama-3.2-1b/", explodingMaterializer(t), checker)

	require.Error(t, err)
	assert.ErrorIs(t, err, checkErr)
}

// TestRunStageWeights_ReStageDifferentSourceReplacesAndReportsPrevious
// covers a re-stage from a new source: the KV entry is replaced with the new
// source/snapshot, and the result reports the previous snapshot ID so an
// operator can reclaim it.
func TestRunStageWeights_ReStageDifferentSourceReplacesAndReportsPrevious(t *testing.T) {
	weightsStore := newWeightsStoreForTest(t)
	ctx := context.Background()
	require.NoError(t, weightsStore.PutWeights(ctx, selfHostModelID, "s3://models/llama-3.2-1b-old/", "snap-old"))

	store := objectstore.NewMemoryObjectStore()
	seedCompleteWeightsPrefix(t, store, "models", "llama-3.2-1b-new/")

	materialize := func(context.Context, string, int64) (string, error) { return "snap-new", nil }

	msg, err := runStageWeights(ctx, store, weightsStore, t.TempDir(),
		selfHostModelID, "s3://models/llama-3.2-1b-new/", materialize, explodingSnapshotChecker(t))

	require.NoError(t, err)
	assert.Contains(t, msg, "snap-new")
	// The previous snapshot ID must appear in the result so an operator can reclaim it.
	assert.Contains(t, msg, "snap-old")

	entry, ok, err := weightsStore.GetWeights(ctx, selfHostModelID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "s3://models/llama-3.2-1b-new/", entry.SourceURI)
	assert.Equal(t, "snap-new", entry.SnapshotID)
}

// TestRunStageWeights_UnknownModelIDRefused covers refusal on a model ID
// absent from the catalog entirely, before any store or materialize work.
func TestRunStageWeights_UnknownModelIDRefused(t *testing.T) {
	weightsStore := newWeightsStoreForTest(t)

	_, err := runStageWeights(context.Background(), explodingObjectStore{t: t}, weightsStore, t.TempDir(),
		"not-a-real-model", "s3://models/llama-3.2-1b/", explodingMaterializer(t), explodingSnapshotChecker(t))

	require.Error(t, err)
	assert.ErrorContains(t, err, "unknown model ID")

	entries, listErr := weightsStore.ListWeights(context.Background())
	require.NoError(t, listErr)
	assert.Empty(t, entries, "refusal must not write a KV entry")
}

// TestRunStageWeights_ProviderModelIDRefused covers refusal on a model ID
// that exists in the catalog but is provider-served, not self-host. The
// error must be distinguishable from the unknown-model-ID case, since
// LookupServingSpec returns found and selfHost separately for exactly this
// reason.
func TestRunStageWeights_ProviderModelIDRefused(t *testing.T) {
	weightsStore := newWeightsStoreForTest(t)

	_, err := runStageWeights(context.Background(), explodingObjectStore{t: t}, weightsStore, t.TempDir(),
		providerModelID, "s3://models/claude/", explodingMaterializer(t), explodingSnapshotChecker(t))

	require.Error(t, err)
	assert.ErrorContains(t, err, "provider-served")
	assert.NotContains(t, err.Error(), "unknown model ID", "must be distinguishable from the unknown-model-ID refusal")

	entries, listErr := weightsStore.ListWeights(context.Background())
	require.NoError(t, listErr)
	assert.Empty(t, entries, "refusal must not write a KV entry")
}

// TestRunStageWeights_MissingRequiredFilesRefusedBeforeMaterializing covers
// refusal on an S3 prefix missing required Hugging Face files: validation
// must fail before download or materialisation is ever attempted.
func TestRunStageWeights_MissingRequiredFilesRefusedBeforeMaterializing(t *testing.T) {
	weightsStore := newWeightsStoreForTest(t)
	store := objectstore.NewMemoryObjectStore()
	// Deliberately incomplete: no config.json, tokenizer files, or safetensors.
	putObject(t, store, "models", "incomplete/README.md", []byte("nothing useful"))

	_, err := runStageWeights(context.Background(), store, weightsStore, t.TempDir(),
		selfHostModelID, "s3://models/incomplete/", explodingMaterializer(t), explodingSnapshotChecker(t))

	require.Error(t, err)
	assert.ErrorContains(t, err, "missing required file")

	entries, listErr := weightsStore.ListWeights(context.Background())
	require.NoError(t, listErr)
	assert.Empty(t, entries, "refusal must not write a KV entry")
}

// TestListWeightsOutput_NoModelsStaged covers the empty-catalog case.
func TestListWeightsOutput_NoModelsStaged(t *testing.T) {
	weightsStore := newWeightsStoreForTest(t)

	out, err := listWeightsOutput(context.Background(), weightsStore, explodingSnapshotChecker(t))
	require.NoError(t, err)
	assert.Equal(t, "No models staged.", out)
}

// TestListWeightsOutput_ListsSeveralStagedModels covers the populated case:
// every staged model's ID, source URI and snapshot ID must appear in the
// rendered output.
func TestListWeightsOutput_ListsSeveralStagedModels(t *testing.T) {
	weightsStore := newWeightsStoreForTest(t)
	ctx := context.Background()
	require.NoError(t, weightsStore.PutWeights(ctx, selfHostModelID, "s3://models/llama-3.2-1b/", "snap-0001"))
	require.NoError(t, weightsStore.PutWeights(ctx, "amazon.titan-text-lite-v1", "s3://models/titan-text-lite/", "snap-0002"))

	out, err := listWeightsOutput(ctx, weightsStore, fixedSnapshotChecker(true))
	require.NoError(t, err)
	assert.Contains(t, out, selfHostModelID)
	assert.Contains(t, out, "s3://models/llama-3.2-1b/")
	assert.Contains(t, out, "snap-0001")
	assert.Contains(t, out, "amazon.titan-text-lite-v1")
	assert.Contains(t, out, "s3://models/titan-text-lite/")
	assert.Contains(t, out, "snap-0002")
}

// TestListWeightsOutput_MarksDanglingEntry covers the annotation this bug is
// about: an entry whose snapshot is gone must be marked dangling, distinctly
// from a live entry, so an operator sees at a glance which staged models can
// actually serve.
func TestListWeightsOutput_MarksDanglingEntry(t *testing.T) {
	weightsStore := newWeightsStoreForTest(t)
	ctx := context.Background()
	require.NoError(t, weightsStore.PutWeights(ctx, selfHostModelID, "s3://models/llama-3.2-1b/", "snap-live"))
	require.NoError(t, weightsStore.PutWeights(ctx, "amazon.titan-text-lite-v1", "s3://models/titan-text-lite/", "snap-gone"))

	checker := func(_ context.Context, snapshotID string) (bool, error) {
		return snapshotID != "snap-gone", nil
	}

	out, err := listWeightsOutput(ctx, weightsStore, checker)
	require.NoError(t, err)

	var liveLine, danglingLine string
	for line := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.Contains(line, "snap-live"):
			liveLine = line
		case strings.Contains(line, "snap-gone"):
			danglingLine = line
		}
	}
	require.NotEmpty(t, liveLine, "live entry must appear in output")
	require.NotEmpty(t, danglingLine, "dangling entry must appear in output")
	assert.NotContains(t, liveLine, weightsStatusDangling, "a live snapshot must not be marked dangling")
	assert.Contains(t, danglingLine, weightsStatusDangling, "a missing snapshot must be marked dangling")
}

// TestRemoveWeights_RoundTrip covers remove: it returns the dropped entry,
// and afterward the model has no staged-weights record. It must not delete
// anything outside the KV bucket -- removeWeights takes no object store at
// all, so there is nothing for it to delete there.
func TestRemoveWeights_RoundTrip(t *testing.T) {
	weightsStore := newWeightsStoreForTest(t)
	ctx := context.Background()
	require.NoError(t, weightsStore.PutWeights(ctx, selfHostModelID, "s3://models/llama-3.2-1b/", "snap-0001"))

	entry, err := removeWeights(ctx, weightsStore, selfHostModelID)
	require.NoError(t, err)
	assert.Equal(t, "snap-0001", entry.SnapshotID)
	assert.Equal(t, "s3://models/llama-3.2-1b/", entry.SourceURI)

	_, ok, err := weightsStore.GetWeights(ctx, selfHostModelID)
	require.NoError(t, err)
	assert.False(t, ok, "KV entry must be dropped")
}

// TestRemoveWeights_UnknownModelIsRefused covers remove against a model with
// no staged entry: a clear error, not a silent no-op.
func TestRemoveWeights_UnknownModelIsRefused(t *testing.T) {
	weightsStore := newWeightsStoreForTest(t)

	_, err := removeWeights(context.Background(), weightsStore, selfHostModelID)
	require.Error(t, err)
	assert.ErrorContains(t, err, "no staged weights entry")
}

// TestBuildWeightsImage_SizesFileAndInvokesRunner covers the real,
// environment-independent half of image building: the image file is
// created and truncated to contentBytes plus 15% overhead, and the runner
// is invoked with the right paths. mkfs.ext4 itself is faked so the test
// does not require it on PATH.
func TestBuildWeightsImage_SizesFileAndInvokesRunner(t *testing.T) {
	origRunner := mkfsExt4Runner
	t.Cleanup(func() { mkfsExt4Runner = origRunner })

	var gotSrcDir, gotImagePath string
	mkfsExt4Runner = func(srcDir, imagePath string) error {
		gotSrcDir = srcDir
		gotImagePath = imagePath
		return nil
	}

	srcDir := t.TempDir()
	imagePath := filepath.Join(t.TempDir(), "vol.img")
	contentBytes := int64(1_000_000_000) // 1 GB: 15% overhead dominates the 64 MiB floor.

	require.NoError(t, buildWeightsImage(srcDir, imagePath, contentBytes))

	assert.Equal(t, srcDir, gotSrcDir)
	assert.Equal(t, imagePath, gotImagePath)

	stat, err := os.Stat(imagePath)
	require.NoError(t, err)
	wantSize := contentBytes + int64(float64(contentBytes)*0.15)
	assert.Equal(t, wantSize, stat.Size())
}

// TestBuildWeightsImage_MinPaddingFloor covers small content: padding is
// floored at 64 MiB rather than the (tiny) 15% overhead.
func TestBuildWeightsImage_MinPaddingFloor(t *testing.T) {
	origRunner := mkfsExt4Runner
	t.Cleanup(func() { mkfsExt4Runner = origRunner })
	mkfsExt4Runner = func(string, string) error { return nil }

	imagePath := filepath.Join(t.TempDir(), "vol.img")
	contentBytes := int64(1024) // 15% of this is far below the 64 MiB floor.

	require.NoError(t, buildWeightsImage(t.TempDir(), imagePath, contentBytes))

	stat, err := os.Stat(imagePath)
	require.NoError(t, err)
	assert.Equal(t, contentBytes+64*1024*1024, stat.Size())
}

// TestBuildWeightsImage_RunnerErrorPropagates covers mkfs.ext4 failing: the
// error must surface, not be swallowed.
func TestBuildWeightsImage_RunnerErrorPropagates(t *testing.T) {
	origRunner := mkfsExt4Runner
	t.Cleanup(func() { mkfsExt4Runner = origRunner })
	mkfsExt4Runner = func(string, string) error { return errors.New("mkfs boom") }

	err := buildWeightsImage(t.TempDir(), filepath.Join(t.TempDir(), "vol.img"), 1024)
	require.Error(t, err)
	assert.ErrorContains(t, err, "mkfs boom")
}

// TestBuildWeightsImage_CreateFileErrorSkipsRunner covers a failure before
// mkfs.ext4 would even run: an unwritable image path must fail without
// invoking the runner.
func TestBuildWeightsImage_CreateFileErrorSkipsRunner(t *testing.T) {
	origRunner := mkfsExt4Runner
	t.Cleanup(func() { mkfsExt4Runner = origRunner })
	called := false
	mkfsExt4Runner = func(string, string) error { called = true; return nil }

	// A path inside a directory that doesn't exist cannot be created.
	badPath := filepath.Join(t.TempDir(), "missing-dir", "vol.img")
	err := buildWeightsImage(t.TempDir(), badPath, 1024)
	require.Error(t, err)
	assert.False(t, called, "runner must not be invoked when the image file cannot be created")
}

// fakeClusterConfig returns a minimal single-node ClusterConfig sufficient
// to drive the ochre weights Run wrappers: all nested config structs are
// value types defaulting to the zero value, which every collaborator on the
// refusal paths (LoadViperblockMasterKey, NewS3ObjectStoreFromConfig) treats
// as "unset", not invalid.
func fakeClusterConfig() *config.ClusterConfig {
	return &config.ClusterConfig{
		Node:  "node1",
		Nodes: map[string]config.Config{"node1": {}},
	}
}

// ochreExitPanic is the sentinel the ochreExit test stand-in panics with, so
// a deferred recover can distinguish "the command called exit" from any
// other panic escaping fn.
type ochreExitPanic struct{ code int }

// withOchreExitCapture swaps ochreExit for a fake that records the exit code
// and panics with ochreExitPanic instead of killing the test binary, runs
// fn, and returns the recorded code (-1 if fn returned without exiting).
// This lets a Run wrapper's real connect/validate/exit control flow be
// exercised directly, the same way real operators experience it.
func withOchreExitCapture(t *testing.T, fn func()) int {
	t.Helper()
	origExit := ochreExit
	t.Cleanup(func() { ochreExit = origExit })

	code := -1
	ochreExit = func(c int) { code = c; panic(ochreExitPanic{c}) }

	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(ochreExitPanic); !ok {
					panic(r)
				}
			}
		}()
		fn()
	}()
	return code
}

// captureStdout redirects os.Stdout for the duration of fn and returns what
// was written, so wrapper tests can assert on the printed result without
// relying on the underlying listWeightsOutput/removeWeights return value
// alone.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stdout
	os.Stdout = w //nolint:reassign // test-local stdout capture, restored below before this func returns

	fn()

	require.NoError(t, w.Close())
	os.Stdout = orig //nolint:reassign // restoring the real os.Stdout captured above
	var buf bytes.Buffer
	_, err = io.Copy(&buf, r)
	require.NoError(t, err)
	return buf.String()
}

// stubConnect swaps loadConfigAndConnectFn for a fake returning cfg/nc/err,
// restoring the real loadConfigAndConnect on cleanup.
func stubConnect(t *testing.T, cfg *config.ClusterConfig, nc *nats.Conn, err error) {
	t.Helper()
	orig := loadConfigAndConnectFn
	t.Cleanup(func() { loadConfigAndConnectFn = orig })
	loadConfigAndConnectFn = func() (*config.ClusterConfig, *nats.Conn, error) { return cfg, nc, err }
}

// newOchreWeightsStageTestCmd sets --model-id/--s3-uri/--tmp-dir on the real
// ochreWeightsStageCmd, so tests drive the actual registered flags rather
// than a hand-rolled stand-in.
func newOchreWeightsStageTestCmd(t *testing.T, modelID, s3URI string) *cobra.Command {
	t.Helper()
	require.NoError(t, ochreWeightsStageCmd.Flags().Set("model-id", modelID))
	require.NoError(t, ochreWeightsStageCmd.Flags().Set("s3-uri", s3URI))
	require.NoError(t, ochreWeightsStageCmd.Flags().Set("tmp-dir", t.TempDir()))
	return ochreWeightsStageCmd
}

func newOchreWeightsRemoveTestCmd(t *testing.T, modelID string) *cobra.Command {
	t.Helper()
	require.NoError(t, ochreWeightsRemoveCmd.Flags().Set("model-id", modelID))
	return ochreWeightsRemoveCmd
}

// TestRunOchreWeightsStage_ConnectFailureExits1 covers the wrapper's first
// failure mode: it cannot even reach the catalog check without a cluster
// connection.
func TestRunOchreWeightsStage_ConnectFailureExits1(t *testing.T) {
	stubConnect(t, nil, nil, errors.New("dial failed"))

	cmd := newOchreWeightsStageTestCmd(t, selfHostModelID, "s3://models/x/")
	code := withOchreExitCapture(t, func() { runOchreWeightsStage(cmd, nil) })
	assert.Equal(t, 1, code)
}

// TestRunOchreWeightsStage_UnknownModelExits1 drives the real cobra Run
// function end to end against an embedded JetStream server, confirming the
// wrapper propagates runStageWeights's catalog refusal into a process exit.
func TestRunOchreWeightsStage_UnknownModelExits1(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	stubConnect(t, fakeClusterConfig(), nc, nil)

	cmd := newOchreWeightsStageTestCmd(t, "not-a-real-model", "s3://models/x/")
	code := withOchreExitCapture(t, func() { runOchreWeightsStage(cmd, nil) })
	assert.Equal(t, 1, code)
}

// TestRunOchreWeightsStage_MasterKeyLoadFailureExits1 covers the wrapper's
// own setup failing (an unreadable viperblock encryption key file) before
// runStageWeights is ever called.
func TestRunOchreWeightsStage_MasterKeyLoadFailureExits1(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	cfg := &config.ClusterConfig{
		Node: "node1",
		Nodes: map[string]config.Config{"node1": {
			Viperblock: config.ViperblockConfig{EncryptionKeyFile: filepath.Join(t.TempDir(), "missing-key")},
		}},
	}
	stubConnect(t, cfg, nc, nil)

	cmd := newOchreWeightsStageTestCmd(t, selfHostModelID, "s3://models/x/")
	code := withOchreExitCapture(t, func() { runOchreWeightsStage(cmd, nil) })
	assert.Equal(t, 1, code)
}

// TestRunOchreWeightsList_ConnectFailureExits1 mirrors the stage wrapper's
// connect-failure coverage for list.
func TestRunOchreWeightsList_ConnectFailureExits1(t *testing.T) {
	stubConnect(t, nil, nil, errors.New("dial failed"))

	code := withOchreExitCapture(t, func() { runOchreWeightsList(nil, nil) })
	assert.Equal(t, 1, code)
}

// notFoundPredastoreServer answers every request with a 404 NoSuchKey S3
// error, so a weightsSnapshotChecker built against it deterministically
// reports a snapshot as gone without needing a real predastore.
func notFoundPredastoreServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+
			`<Error><Code>NoSuchKey</Code>`+
			`<Message>The specified key does not exist.</Message></Error>`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRunOchreWeightsList_PrintsStagedModels drives the real Run function
// end to end: it must print what a separately-constructed WeightsStore
// against the same embedded JetStream server sees, and mark the entry
// dangling since the mock predastore below has no such snapshot.
func TestRunOchreWeightsList_PrintsStagedModels(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	srv := notFoundPredastoreServer(t)
	cfg := &config.ClusterConfig{
		Node: "node1",
		Nodes: map[string]config.Config{
			"node1": {Predastore: config.PredastoreConfig{Host: srv.URL, Region: "us-east-1", Bucket: "test-bucket", AccessKey: "test", SecretKey: "test"}},
		},
	}
	stubConnect(t, cfg, nc, nil)

	seedStore := gateway_bedrock.NewWeightsStore(js, 1)
	require.NoError(t, seedStore.PutWeights(context.Background(), selfHostModelID, "s3://models/llama-3.2-1b/", "snap-0001"))

	out := captureStdout(t, func() { runOchreWeightsList(nil, nil) })
	assert.Contains(t, out, selfHostModelID)
	assert.Contains(t, out, "snap-0001")
	assert.Contains(t, out, weightsStatusDangling, "snapshot absent from the mock predastore must be marked dangling")
}

// TestRunOchreWeightsList_PrintsNoModelsStaged covers the empty case through
// the full wrapper.
func TestRunOchreWeightsList_PrintsNoModelsStaged(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	stubConnect(t, fakeClusterConfig(), nc, nil)

	out := captureStdout(t, func() { runOchreWeightsList(nil, nil) })
	assert.Contains(t, out, "No models staged.")
}

// TestRunOchreWeightsRemove_ConnectFailureExits1 mirrors the stage wrapper's
// connect-failure coverage for remove.
func TestRunOchreWeightsRemove_ConnectFailureExits1(t *testing.T) {
	stubConnect(t, nil, nil, errors.New("dial failed"))

	cmd := newOchreWeightsRemoveTestCmd(t, selfHostModelID)
	code := withOchreExitCapture(t, func() { runOchreWeightsRemove(cmd, nil) })
	assert.Equal(t, 1, code)
}

// TestRunOchreWeightsRemove_UnknownModelExits1 drives the real cobra Run
// function against an embedded JetStream server with nothing staged.
func TestRunOchreWeightsRemove_UnknownModelExits1(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	stubConnect(t, fakeClusterConfig(), nc, nil)

	cmd := newOchreWeightsRemoveTestCmd(t, selfHostModelID)
	code := withOchreExitCapture(t, func() { runOchreWeightsRemove(cmd, nil) })
	assert.Equal(t, 1, code)
}

// TestRunOchreWeightsRemove_RoundTripPrintsSnapshotAndDropsEntry drives the
// full wrapper: it must print the removed snapshot ID and leave the model
// with no staged-weights entry afterward.
func TestRunOchreWeightsRemove_RoundTripPrintsSnapshotAndDropsEntry(t *testing.T) {
	ns, nc, js := testutil.StartTestJetStream(t)
	stubConnect(t, fakeClusterConfig(), nc, nil)

	seedStore := gateway_bedrock.NewWeightsStore(js, 1)
	require.NoError(t, seedStore.PutWeights(context.Background(), selfHostModelID, "s3://models/llama-3.2-1b/", "snap-0001"))

	cmd := newOchreWeightsRemoveTestCmd(t, selfHostModelID)
	out := captureStdout(t, func() { runOchreWeightsRemove(cmd, nil) })
	assert.Contains(t, out, "snap-0001")

	// runOchreWeightsRemove closed nc on return (mirroring real CLI usage: one
	// connection per invocation), so verify the drop over a fresh connection
	// to the same embedded server rather than reusing the closed one.
	verifyNC, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { verifyNC.Close() })
	verifyStore := gateway_bedrock.NewWeightsStore(testutil.NewJetStream(t, verifyNC), 1)

	_, ok, err := verifyStore.GetWeights(context.Background(), selfHostModelID)
	require.NoError(t, err)
	assert.False(t, ok, "KV entry must be dropped")
}

// failingObjectStore wraps a MemoryObjectStore and forces selected
// operations to fail with a generic (non-NoSuchKey) error, covering error
// branches a well-formed prefix never exercises: a transport/permission
// failure, as opposed to an absent key.
type failingObjectStore struct {
	*objectstore.MemoryObjectStore

	failListObjects bool
	failGetObject   bool
	failPutObject   bool
	// failHeadObjectKeys, if set, fails HeadObject only for these keys.
	failHeadObjectKeys map[string]bool
}

func (f *failingObjectStore) PutObject(ctx context.Context, input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	if f.failPutObject {
		return nil, errors.New("put boom")
	}
	return f.MemoryObjectStore.PutObject(ctx, input)
}

func (f *failingObjectStore) ListObjectsV2(ctx context.Context, input *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	if f.failListObjects {
		return nil, errors.New("list boom")
	}
	return f.MemoryObjectStore.ListObjectsV2(ctx, input)
}

func (f *failingObjectStore) GetObject(ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if f.failGetObject {
		return nil, errors.New("get boom")
	}
	return f.MemoryObjectStore.GetObject(ctx, input)
}

func (f *failingObjectStore) HeadObject(ctx context.Context, input *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
	if f.failHeadObjectKeys[aws.StringValue(input.Key)] {
		return nil, errors.New("head boom")
	}
	return f.MemoryObjectStore.HeadObject(ctx, input)
}

var _ objectstore.ObjectStore = (*failingObjectStore)(nil)

// pagingObjectStore returns ListObjectsV2 results across two pages linked by
// a continuation token, covering listWeightsPrefix's pagination loop, which
// MemoryObjectStore's single-page response never exercises.
type pagingObjectStore struct {
	*objectstore.MemoryObjectStore

	pages []*s3.Object
	calls int
}

func (p *pagingObjectStore) ListObjectsV2(context.Context, *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	obj := p.pages[p.calls]
	p.calls++
	out := &s3.ListObjectsV2Output{Contents: []*s3.Object{obj}}
	if p.calls < len(p.pages) {
		truncated := true
		token := "next"
		out.IsTruncated = &truncated
		out.NextContinuationToken = &token
	}
	return out, nil
}

// TestListWeightsPrefix_PaginatesAcrossContinuationToken covers the
// continuation-token loop: every page must be walked, not just the first.
func TestListWeightsPrefix_PaginatesAcrossContinuationToken(t *testing.T) {
	store := &pagingObjectStore{
		MemoryObjectStore: objectstore.NewMemoryObjectStore(),
		pages: []*s3.Object{
			{Key: aws.String("a")},
			{Key: aws.String("b")},
		},
	}

	var got []string
	err := listWeightsPrefix(context.Background(), store, "models", "prefix/", func(obj *s3.Object) error {
		got = append(got, aws.StringValue(obj.Key))
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, got)
	assert.Equal(t, 2, store.calls)
}

// TestListWeightsPrefix_ListObjectsErrorPropagates covers a transport error
// from ListObjectsV2 itself, as opposed to a missing key.
func TestListWeightsPrefix_ListObjectsErrorPropagates(t *testing.T) {
	store := &failingObjectStore{MemoryObjectStore: objectstore.NewMemoryObjectStore(), failListObjects: true}

	err := listWeightsPrefix(context.Background(), store, "models", "prefix/", func(*s3.Object) error { return nil })
	require.Error(t, err)
	assert.ErrorContains(t, err, "list boom")
}

// TestListWeightsPrefix_CallbackErrorStopsWalk covers fn's error aborting
// the walk immediately, rather than being swallowed.
func TestListWeightsPrefix_CallbackErrorStopsWalk(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	putObject(t, store, "models", "prefix/a", nil)

	err := listWeightsPrefix(context.Background(), store, "models", "prefix/", func(*s3.Object) error {
		return errors.New("callback boom")
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "callback boom")
}

// TestValidateWeightsPrefix_RequiredFileHeadErrorPropagates covers a
// transport error checking a fixed-name required file, distinct from that
// file simply being absent (objectstore.IsNoSuchKeyError).
func TestValidateWeightsPrefix_RequiredFileHeadErrorPropagates(t *testing.T) {
	store := &failingObjectStore{
		MemoryObjectStore:  objectstore.NewMemoryObjectStore(),
		failHeadObjectKeys: map[string]bool{"llama-3.2-1b/config.json": true},
	}

	err := validateWeightsPrefix(context.Background(), store, "models", "llama-3.2-1b/")
	require.Error(t, err)
	assert.ErrorContains(t, err, "head boom")
}

// TestValidateWeightsPrefix_SafetensorsListErrorPropagates covers a
// transport error listing for *.safetensors, distinct from none being
// present.
func TestValidateWeightsPrefix_SafetensorsListErrorPropagates(t *testing.T) {
	store := &failingObjectStore{MemoryObjectStore: objectstore.NewMemoryObjectStore(), failListObjects: true}
	for _, name := range requiredWeightsFiles {
		putObject(t, store.MemoryObjectStore, "models", "llama-3.2-1b/"+name, nil)
	}

	err := validateWeightsPrefix(context.Background(), store, "models", "llama-3.2-1b/")
	require.Error(t, err)
	assert.ErrorContains(t, err, "list boom")
}

// TestValidateWeightsPrefix_TokenizerHeadErrorPropagates covers a transport
// error checking the tokenizer files, distinct from neither being present.
func TestValidateWeightsPrefix_TokenizerHeadErrorPropagates(t *testing.T) {
	store := &failingObjectStore{
		MemoryObjectStore: objectstore.NewMemoryObjectStore(),
		failHeadObjectKeys: map[string]bool{
			"llama-3.2-1b/tokenizer.json":  true,
			"llama-3.2-1b/tokenizer.model": true,
		},
	}
	for _, name := range requiredWeightsFiles {
		putObject(t, store.MemoryObjectStore, "models", "llama-3.2-1b/"+name, nil)
	}
	putObject(t, store.MemoryObjectStore, "models", "llama-3.2-1b/model.safetensors", nil)

	err := validateWeightsPrefix(context.Background(), store, "models", "llama-3.2-1b/")
	require.Error(t, err)
	assert.ErrorContains(t, err, "head boom")
}

// TestDownloadWeightsPrefix_SkipsDirectoryMarkers covers predastore
// directory-marker keys (ending in '/'): they must be skipped, not
// downloaded as zero-byte files.
func TestDownloadWeightsPrefix_SkipsDirectoryMarkers(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	putObject(t, store, "models", "llama-3.2-1b/subdir/", nil)
	putObject(t, store, "models", "llama-3.2-1b/config.json", []byte("{}"))

	destDir := t.TempDir()
	total, err := downloadWeightsPrefix(context.Background(), store, "models", "llama-3.2-1b/", destDir)
	require.NoError(t, err)
	assert.Equal(t, int64(len("{}")), total)

	entries, err := os.ReadDir(destDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "config.json", entries[0].Name())
}

// TestDownloadWeightsPrefix_GetObjectErrorPropagates covers a download
// failure surfacing through downloadWeightsPrefix's walk, not being
// swallowed.
func TestDownloadWeightsPrefix_GetObjectErrorPropagates(t *testing.T) {
	store := &failingObjectStore{MemoryObjectStore: objectstore.NewMemoryObjectStore(), failGetObject: true}
	putObject(t, store.MemoryObjectStore, "models", "llama-3.2-1b/config.json", []byte("{}"))

	_, err := downloadWeightsPrefix(context.Background(), store, "models", "llama-3.2-1b/", t.TempDir())
	require.Error(t, err)
	assert.ErrorContains(t, err, "get boom")
}

// TestDownloadObjectTo_OpenFileErrorPropagates covers the destination file
// being uncreatable (parent directory missing): the error must surface
// before any copy is attempted.
func TestDownloadObjectTo_OpenFileErrorPropagates(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	putObject(t, store, "models", "llama-3.2-1b/config.json", []byte("{}"))

	badDest := filepath.Join(t.TempDir(), "missing-dir", "config.json")
	_, err := downloadObjectTo(context.Background(), store, "models", "llama-3.2-1b/config.json", badDest)
	require.Error(t, err)
}

// lookupMkfsExt4 must find e2fsprogs in sbin, which a non-login shell's PATH
// routinely omits. PATH is emptied so the fallback is what is under test.
func TestLookupMkfsExt4_FallsBackToSbinDir(t *testing.T) {
	sbin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(sbin, "mkfs.ext4"), []byte("#!/bin/sh\n"), 0o755))

	t.Setenv("PATH", "")
	prev := mkfsExt4SbinDirs
	mkfsExt4SbinDirs = []string{sbin}
	t.Cleanup(func() { mkfsExt4SbinDirs = prev })

	path, err := lookupMkfsExt4()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(sbin, "mkfs.ext4"), path)
}

func TestLookupMkfsExt4_AbsentEverywhereIsError(t *testing.T) {
	t.Setenv("PATH", "")
	prev := mkfsExt4SbinDirs
	mkfsExt4SbinDirs = []string{t.TempDir()}
	t.Cleanup(func() { mkfsExt4SbinDirs = prev })

	_, err := lookupMkfsExt4()
	require.Error(t, err)
	require.Contains(t, err.Error(), "install e2fsprogs")
}

// A directory named mkfs.ext4 must not satisfy the lookup.
func TestLookupMkfsExt4_IgnoresDirectory(t *testing.T) {
	sbin := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(sbin, "mkfs.ext4"), 0o755))

	t.Setenv("PATH", "")
	prev := mkfsExt4SbinDirs
	mkfsExt4SbinDirs = []string{sbin}
	t.Cleanup(func() { mkfsExt4SbinDirs = prev })

	_, err := lookupMkfsExt4()
	require.Error(t, err)
}

// The launcher clones the weights snapshot through the EC2 control plane, so
// what stage writes must be readable by the same helper CreateVolume reads
// with, and carry the two fields it consumes: VolumeID (the viperblock source
// prefix) and VolumeSize (compared against a requested size, so GiB).
func TestRegisterWeightsSnapshot_WritesEC2ReadableMetadata(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const bucket = "predastore"
	require.NoError(t, registerWeightsSnapshot(store, bucket,
		"snap-vol-abc", "vol-abc", 12*bytesPerGiB, "ap-southeast-2a", true))

	cfg, err := handlers_ec2_snapshot.ReadSnapshotConfig(store, bucket, "snap-vol-abc")
	require.NoError(t, err)
	assert.Equal(t, "snap-vol-abc", cfg.SnapshotID)
	assert.Equal(t, "vol-abc", cfg.VolumeID)
	assert.Equal(t, int64(12), cfg.VolumeSize)
	assert.Equal(t, "completed", cfg.State)
	assert.Equal(t, "ap-southeast-2a", cfg.AvailabilityZone)
	assert.True(t, cfg.Encrypted)
}

// An unencrypted volume must not be advertised as encrypted, which would let a
// consumer skip a key it actually needs.
func TestRegisterWeightsSnapshot_UnencryptedVolume(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	require.NoError(t, registerWeightsSnapshot(store, "predastore",
		"snap-vol-plain", "vol-plain", bytesPerGiB, "ap-southeast-2a", false))

	cfg, err := handlers_ec2_snapshot.ReadSnapshotConfig(store, "predastore", "snap-vol-plain")
	require.NoError(t, err)
	assert.False(t, cfg.Encrypted)
}

// A failed metadata write must surface, not leave stage reporting a snapshot
// ID the launcher will later reject as not found.
func TestRegisterWeightsSnapshot_WriteFailureSurfaces(t *testing.T) {
	store := &failingObjectStore{MemoryObjectStore: objectstore.NewMemoryObjectStore(), failPutObject: true}
	err := registerWeightsSnapshot(store, "predastore",
		"snap-vol-abc", "vol-abc", bytesPerGiB, "ap-southeast-2a", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "register snapshot metadata")
}

// ochreFlagCmd builds a bare command carrying the model-selection flags, so
// the resolver can be exercised without the cluster connection the real
// subcommands make.
func ochreFlagCmd(t *testing.T, modelID string, allModels bool) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().String("model-id", modelID, "")
	cmd.Flags().Bool("all-models", allModels, "")
	return cmd
}

func TestOchreTargetModels_SingleModel(t *testing.T) {
	models, err := ochreTargetModels(ochreFlagCmd(t, selfHostModelID, false))
	require.NoError(t, err)
	assert.Equal(t, []string{selfHostModelID}, models)
}

func TestOchreTargetModels_AllModels(t *testing.T) {
	models, err := ochreTargetModels(ochreFlagCmd(t, "", true))
	require.NoError(t, err)
	assert.Equal(t, gateway_bedrock.CatalogModelIDs(), models)
	assert.NotEmpty(t, models)
}

// TestOchreTargetModels_NeitherFlag pins the refusal to guess: silently
// defaulting to the whole catalog would over-grant on a typo.
func TestOchreTargetModels_NeitherFlag(t *testing.T) {
	_, err := ochreTargetModels(ochreFlagCmd(t, "", false))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--model-id or --all-models")
}

func TestOchreTargetModels_BothFlags(t *testing.T) {
	_, err := ochreTargetModels(ochreFlagCmd(t, selfHostModelID, true))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

// ochreAccessCmd builds a command carrying the access subcommands' full flag
// set, so the grant/revoke/list runners can be driven without the cobra
// command tree.
func ochreAccessCmd(t *testing.T, accountID, modelID string, allModels bool) *cobra.Command {
	t.Helper()
	cmd := ochreFlagCmd(t, modelID, allModels)
	cmd.Flags().String("account-id", accountID, "")
	return cmd
}

const accessTestAccount = "000000000001"

// stubAccessConnect dials a fresh connection per call, because each access
// runner closes the connection it was handed; a single shared one would leave
// the next runner in a sequence talking to a closed conn.
func stubAccessConnect(t *testing.T, clientURL string) {
	t.Helper()
	orig := loadConfigAndConnectFn
	t.Cleanup(func() { loadConfigAndConnectFn = orig })
	loadConfigAndConnectFn = func() (*config.ClusterConfig, *nats.Conn, error) {
		nc, err := nats.Connect(clientURL)
		if err != nil {
			return nil, nil, err
		}
		t.Cleanup(nc.Close)
		return fakeClusterConfig(), nc, nil
	}
}

// TestRunOchreAccessChange_ModelFlagFailureExits1 pins that the flag refusal
// happens before any cluster connection: an operator who mistypes a flag gets
// told so without a NATS dial.
func TestRunOchreAccessChange_ModelFlagFailureExits1(t *testing.T) {
	stubConnect(t, nil, nil, errors.New("connect must not be reached"))

	cmd := ochreAccessCmd(t, accessTestAccount, "", false)
	code := withOchreExitCapture(t, func() { runOchreAccessGrant(cmd, nil) })
	assert.Equal(t, 1, code)
}

func TestRunOchreAccessGrant_ConnectFailureExits1(t *testing.T) {
	stubConnect(t, nil, nil, errors.New("dial failed"))

	cmd := ochreAccessCmd(t, accessTestAccount, selfHostModelID, false)
	code := withOchreExitCapture(t, func() { runOchreAccessGrant(cmd, nil) })
	assert.Equal(t, 1, code)
}

func TestRunOchreAccessList_ConnectFailureExits1(t *testing.T) {
	stubConnect(t, nil, nil, errors.New("dial failed"))

	cmd := ochreAccessCmd(t, accessTestAccount, "", false)
	code := withOchreExitCapture(t, func() { runOchreAccessList(cmd, nil) })
	assert.Equal(t, 1, code)
}

// TestRunOchreAccessGrantListRevoke drives the three runners in sequence
// against an embedded JetStream server, so the CLI is checked against the same
// grant store the gateway resolves against rather than a fake.
func TestRunOchreAccessGrantListRevoke(t *testing.T) {
	srv, _, _ := testutil.StartTestJetStream(t)
	stubAccessConnect(t, srv.ClientURL())

	grantCmd := ochreAccessCmd(t, accessTestAccount, selfHostModelID, false)
	out := captureStdout(t, func() { runOchreAccessGrant(grantCmd, nil) })
	assert.Contains(t, out, "Granted")
	assert.Contains(t, out, selfHostModelID)

	listCmd := ochreAccessCmd(t, accessTestAccount, "", false)
	out = captureStdout(t, func() { runOchreAccessList(listCmd, nil) })
	assert.Contains(t, out, selfHostModelID)
	assert.NotContains(t, out, providerModelID, "list must show only the granted model")

	revokeCmd := ochreAccessCmd(t, accessTestAccount, selfHostModelID, false)
	out = captureStdout(t, func() { runOchreAccessRevoke(revokeCmd, nil) })
	assert.Contains(t, out, "Revoked")

	out = captureStdout(t, func() { runOchreAccessList(listCmd, nil) })
	assert.Contains(t, out, "no model grants")
}

// TestRunOchreAccessGrant_AllModelsGrantsWholeCatalog covers the --all-models
// fan-out, which is what provisioning uses.
func TestRunOchreAccessGrant_AllModelsGrantsWholeCatalog(t *testing.T) {
	srv, _, _ := testutil.StartTestJetStream(t)
	stubAccessConnect(t, srv.ClientURL())

	grantCmd := ochreAccessCmd(t, accessTestAccount, "", true)
	captureStdout(t, func() { runOchreAccessGrant(grantCmd, nil) })

	listCmd := ochreAccessCmd(t, accessTestAccount, "", false)
	out := captureStdout(t, func() { runOchreAccessList(listCmd, nil) })
	for _, modelID := range gateway_bedrock.CatalogModelIDs() {
		assert.Contains(t, out, modelID)
	}
}

// TestRunOchreAccessList_OtherAccountSeesNothing is the deny-by-default rule
// as an operator observes it: an account nobody granted has an empty listing.
func TestRunOchreAccessList_OtherAccountSeesNothing(t *testing.T) {
	srv, _, _ := testutil.StartTestJetStream(t)
	stubAccessConnect(t, srv.ClientURL())

	grantCmd := ochreAccessCmd(t, accessTestAccount, selfHostModelID, false)
	captureStdout(t, func() { runOchreAccessGrant(grantCmd, nil) })

	listCmd := ochreAccessCmd(t, "000000000002", "", false)
	out := captureStdout(t, func() { runOchreAccessList(listCmd, nil) })
	assert.Contains(t, out, "no model grants")
}

// TestRunOchreModels_PrintsSortedCatalog checks the listing operators consult
// before granting: every catalog ID, one per line.
func TestRunOchreModels_PrintsSortedCatalog(t *testing.T) {
	out := captureStdout(t, func() { runOchreModels(nil, nil) })
	for _, modelID := range gateway_bedrock.CatalogModelIDs() {
		assert.Contains(t, out, modelID)
	}
}
