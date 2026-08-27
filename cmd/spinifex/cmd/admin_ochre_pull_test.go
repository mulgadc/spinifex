package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	"github.com/mulgadc/spinifex/spinifex/gateway/bedrock/hfhub"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- selectPullFiles / anySafetensors (D4) ---

// TestSelectPullFiles_SafetensorsAndConfigOnly covers D4's filter: every
// *.safetensors file, the fixed config set (root only), tokenizer files (at
// any depth, so Llama's original/tokenizer.model qualifies), and the shard
// index are kept; a pickle .bin, a directory entry, and a nested config.json
// (a sentence-transformers component config that must never flatten onto the
// model's own config.json) are dropped.
func TestSelectPullFiles_SafetensorsAndConfigOnly(t *testing.T) {
	entries := []hfhub.TreeEntry{
		{Type: "file", Path: "config.json"},
		{Type: "file", Path: "model-00001-of-00002.safetensors"},
		{Type: "file", Path: "model-00002-of-00002.safetensors"},
		{Type: "file", Path: "tokenizer_config.json"},
		{Type: "file", Path: "tokenizer.json"},
		{Type: "file", Path: "pytorch_model.bin"},
		{Type: "file", Path: "model.safetensors.index.json"},
		{Type: "directory", Path: "original"},
		{Type: "file", Path: "original/tokenizer.model"},
		{Type: "file", Path: "1_Pooling/config.json"},
	}

	selected := selectPullFiles(entries)

	var paths []string
	for _, e := range selected {
		paths = append(paths, e.Path)
	}
	assert.ElementsMatch(t, []string{
		"config.json",
		"model-00001-of-00002.safetensors",
		"model-00002-of-00002.safetensors",
		"tokenizer_config.json",
		"tokenizer.json",
		"model.safetensors.index.json",
		"original/tokenizer.model",
	}, paths)
	assert.NotContains(t, paths, "1_Pooling/config.json", "a nested config.json must be dropped, never flattened onto the model config")
}

// TestAnySafetensors_TrueWhenPresent covers the pass case: at least one
// selected file is a safetensors shard.
func TestAnySafetensors_TrueWhenPresent(t *testing.T) {
	assert.True(t, anySafetensors([]hfhub.TreeEntry{{Type: "file", Path: "model.safetensors"}}))
}

// TestAnySafetensors_FalseWhenOnlyPickleAndConfig covers D4's abort trigger:
// a repo whose only weights are pickle .bin, even with config/tokenizer
// files also selected, has nothing pull will actually download as weights.
func TestAnySafetensors_FalseWhenOnlyPickleAndConfig(t *testing.T) {
	selected := selectPullFiles([]hfhub.TreeEntry{
		{Type: "file", Path: "config.json"},
		{Type: "file", Path: "pytorch_model.bin"},
	})
	assert.False(t, anySafetensors(selected))
}

// --- defaultPullPrefix (D7) ---

// TestDefaultPullPrefix_KeyedByRepoAndSHA covers the fallback destination
// when --s3-uri is omitted: immutable and self-deduping, keyed by the exact
// repo and resolved commit SHA.
func TestDefaultPullPrefix_KeyedByRepoAndSHA(t *testing.T) {
	got := defaultPullPrefix("meta-llama/Llama-3.2-1B-Instruct", "abc123def456")
	assert.Equal(t, "s3://ochre-weights/meta-llama/Llama-3.2-1B-Instruct/abc123def456/", got)
}

// --- ochre-pull.json manifest (D3) ---

// TestPullManifest_RoundTrips covers put then read of the manifest 'pull'
// writes and 'stage' reads back.
func TestPullManifest_RoundTrips(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	ctx := context.Background()
	want := ochrePullManifest{HFRepo: "meta-llama/Llama-3.2-1B-Instruct", RevisionSHA: "abc123", PulledAt: time.Now().UTC().Truncate(time.Second)}

	require.NoError(t, putPullManifest(ctx, store, "ochre-weights", "meta-llama/Llama-3.2-1B-Instruct/abc123/", want))

	got, ok, err := readPullManifest(ctx, store, "ochre-weights", "meta-llama/Llama-3.2-1B-Instruct/abc123/")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, want.HFRepo, got.HFRepo)
	assert.Equal(t, want.RevisionSHA, got.RevisionSHA)
	assert.WithinDuration(t, want.PulledAt, got.PulledAt, time.Second)
}

// TestReadPullManifest_MissingIsNotError covers stage's offline path: a
// prefix with no manifest must report ok=false, not an error.
func TestReadPullManifest_MissingIsNotError(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	_, ok, err := readPullManifest(context.Background(), store, "ochre-weights", "some/prefix/")
	require.NoError(t, err)
	assert.False(t, ok)
}

// --- runStageWeights x manifest integration ---

// TestRunStageWeights_RecordsSourceRevisionFromManifest covers D3's stage
// side: when the source prefix carries a pull manifest, stage must record
// its commit SHA against the model.
func TestRunStageWeights_RecordsSourceRevisionFromManifest(t *testing.T) {
	weightsStore := newWeightsStoreForTest(t)
	ctx := context.Background()

	store := objectstore.NewMemoryObjectStore()
	seedCompleteWeightsPrefix(t, store, "models", "llama-3.2-1b/")
	require.NoError(t, putPullManifest(ctx, store, "models", "llama-3.2-1b/", ochrePullManifest{
		HFRepo: "meta-llama/Llama-3.2-1B-Instruct", RevisionSHA: "abc123def456", PulledAt: time.Now().UTC(),
	}))

	materialize := func(context.Context, string, int64) (string, error) { return "snap-0001", nil }
	_, err := runStageWeights(ctx, store, weightsStore, t.TempDir(), selfHostModelID, "s3://models/llama-3.2-1b/", materialize, explodingSnapshotChecker(t))
	require.NoError(t, err)

	entry, ok, err := weightsStore.GetWeights(ctx, selfHostModelID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "abc123def456", entry.SourceRevision)
}

// TestRunStageWeights_NoManifestLeavesSourceRevisionEmpty covers the offline
// path explicitly: an operator-supplied prefix with no pull manifest stages
// fine and records an empty SourceRevision, not an error or a fabricated one.
func TestRunStageWeights_NoManifestLeavesSourceRevisionEmpty(t *testing.T) {
	weightsStore := newWeightsStoreForTest(t)
	ctx := context.Background()

	store := objectstore.NewMemoryObjectStore()
	seedCompleteWeightsPrefix(t, store, "models", "llama-3.2-1b/")

	materialize := func(context.Context, string, int64) (string, error) { return "snap-0001", nil }
	_, err := runStageWeights(ctx, store, weightsStore, t.TempDir(), selfHostModelID, "s3://models/llama-3.2-1b/", materialize, explodingSnapshotChecker(t))
	require.NoError(t, err)

	entry, ok, err := weightsStore.GetWeights(ctx, selfHostModelID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Empty(t, entry.SourceRevision)
}

// --- fake Hugging Face server ---

type fakeHFFile struct {
	path    string
	content string
}

// newFakeHFServer serves a Hugging Face-shaped API for repo@revision
// resolving to sha: revision resolution, a recursive tree listing built from
// files, and each file's content at /resolve/<sha>/<path>.
func newFakeHFServer(t *testing.T, repo, revision, sha string, files []fakeHFFile) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/api/models/%s/revision/%s", repo, revision), func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": sha})
	})
	mux.HandleFunc(fmt.Sprintf("/api/models/%s/tree/%s", repo, sha), func(w http.ResponseWriter, _ *http.Request) {
		entries := make([]hfhub.TreeEntry, len(files))
		for i, f := range files {
			entries[i] = hfhub.TreeEntry{Type: "file", Path: f.path, Size: int64(len(f.content))}
		}
		_ = json.NewEncoder(w).Encode(entries)
	})
	for _, f := range files {
		mux.HandleFunc(fmt.Sprintf("/%s/resolve/%s/%s", repo, sha, f.path), func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, f.content)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// gatedHFServer answers every request with status, simulating a gated repo
// with no usable token (D5).
func gatedHFServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// deadHFClient is a Client pointed at an address nothing listens on, so a
// path that must never touch the network fails fast instead of reaching the
// real Hugging Face hub.
func deadHFClient() *hfhub.Client {
	return &hfhub.Client{BaseURL: "http://127.0.0.1:1", HTTP: &http.Client{Timeout: time.Second}}
}

const (
	testHFRepo = "meta-llama/Llama-3.2-1B-Instruct"
	testHFSHA  = "abc123def456"
)

// --- runPullWeights (core logic) ---

// TestRunPullWeights_UnknownModelRefused covers the catalog gate: an
// unrecognised model ID must refuse before any Hugging Face or S3 work.
func TestRunPullWeights_UnknownModelRefused(t *testing.T) {
	_, err := runPullWeights(context.Background(), deadHFClient(), explodingObjectStore{t: t}, "not-a-real-model", testHFRepo, "main", true, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not present in the Ochre catalog")
}

// TestWeightsStagingRefusal_PullOperationWording covers the self-host gate's
// message for the 'pull' operation. v1 ships no provider-tier catalog entry,
// so this drives weightsStagingRefusal directly rather than through a real
// LookupServingSpec lookup.
func TestWeightsStagingRefusal_PullOperationWording(t *testing.T) {
	err := weightsStagingRefusal(providerModelID, "pull", true, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not self-host")
}

// TestRunPullWeights_ResolvesUploadsAndWritesManifest is the happy path: it
// resolves main to a pinned SHA, uploads only the safetensors + config set
// flattened under the default prefix (D7), skips the pickle checkpoint, and
// writes the ochre-pull.json manifest (D3).
func TestRunPullWeights_ResolvesUploadsAndWritesManifest(t *testing.T) {
	files := []fakeHFFile{
		{"config.json", "{}"},
		{"tokenizer_config.json", "{}"},
		{"tokenizer.json", "{}"},
		{"model.safetensors", "fake-weights-bytes"},
		{"pytorch_model.bin", "must-not-be-uploaded"},
	}
	srv := newFakeHFServer(t, testHFRepo, "main", testHFSHA, files)
	hf := &hfhub.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	store := objectstore.NewMemoryObjectStore()

	finalURI, err := runPullWeights(context.Background(), hf, store, selfHostModelID, testHFRepo, "main", true, "")
	require.NoError(t, err)
	assert.Equal(t, defaultPullPrefix(testHFRepo, testHFSHA), finalURI)

	bucket, prefix, err := parseWeightsS3URI(finalURI)
	require.NoError(t, err)

	out, err := store.GetObject(context.Background(), &s3.GetObjectInput{Bucket: strPtr(bucket), Key: strPtr(prefix + "model.safetensors")})
	require.NoError(t, err)
	got, _ := io.ReadAll(out.Body)
	assert.Equal(t, "fake-weights-bytes", string(got))

	_, err = store.GetObject(context.Background(), &s3.GetObjectInput{Bucket: strPtr(bucket), Key: strPtr(prefix + "pytorch_model.bin")})
	assert.True(t, objectstore.IsNoSuchKeyError(err), "pickle checkpoint must never be uploaded")

	manifest, ok, err := readPullManifest(context.Background(), store, bucket, prefix)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, testHFRepo, manifest.HFRepo)
	assert.Equal(t, testHFSHA, manifest.RevisionSHA)
}

// TestRunPullWeights_ExplicitS3URIOverridesDefault covers D7's primary path:
// an operator-named --s3-uri is used verbatim (normalised) instead of the
// keyed default.
func TestRunPullWeights_ExplicitS3URIOverridesDefault(t *testing.T) {
	files := []fakeHFFile{{"config.json", "{}"}, {"tokenizer_config.json", "{}"}, {"tokenizer.json", "{}"}, {"model.safetensors", "x"}}
	srv := newFakeHFServer(t, testHFRepo, "main", testHFSHA, files)
	hf := &hfhub.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	store := objectstore.NewMemoryObjectStore()

	finalURI, err := runPullWeights(context.Background(), hf, store, selfHostModelID, testHFRepo, "main", true, "s3://custom-bucket/my-prefix")
	require.NoError(t, err)
	assert.Equal(t, "s3://custom-bucket/my-prefix/", finalURI)
}

// TestRunPullWeights_NoSafetensorsAborts covers D4's abort case: a repo with
// only pickle checkpoints must be refused before any object lands.
func TestRunPullWeights_NoSafetensorsAborts(t *testing.T) {
	files := []fakeHFFile{{"config.json", "{}"}, {"pytorch_model.bin", "x"}}
	srv := newFakeHFServer(t, testHFRepo, "main", testHFSHA, files)
	hf := &hfhub.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	store := objectstore.NewMemoryObjectStore()

	_, err := runPullWeights(context.Background(), hf, store, selfHostModelID, testHFRepo, "main", true, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no *.safetensors")
	assert.Equal(t, 0, store.Count(), "a refused pull must land nothing")
}

// TestRunPullWeights_GatedRepoAbortsCleanBeforeAnyUpload covers D5: a
// 401/403 from the hub must fail before any object lands, with a clear
// awserrors-coded licence/credential error.
func TestRunPullWeights_GatedRepoAbortsCleanBeforeAnyUpload(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := gatedHFServer(t, status)
		hf := &hfhub.Client{BaseURL: srv.URL, HTTP: srv.Client()}
		store := objectstore.NewMemoryObjectStore()

		_, err := runPullWeights(context.Background(), hf, store, selfHostModelID, testHFRepo, "main", true, "")
		require.Error(t, err)
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorAccessDeniedException), "status %d: got %v", status, err)
		assert.Equal(t, 0, store.Count(), "a gated pull must land nothing")
	}
}

// failAfterNPutsStore wraps a MemoryObjectStore, succeeding the first n
// PutObject calls and failing every one after, so a mid-pull failure and its
// cleanup (D5) are exercised deterministically.
type failAfterNPutsStore struct {
	*objectstore.MemoryObjectStore

	n     int
	calls int
}

func (f *failAfterNPutsStore) PutObject(ctx context.Context, input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	f.calls++
	if f.calls > f.n {
		return nil, errors.New("simulated upload failure")
	}
	return f.MemoryObjectStore.PutObject(ctx, input)
}

var _ objectstore.ObjectStore = (*failAfterNPutsStore)(nil)

// TestRunPullWeights_MidStreamFailureCleansUpPartialPrefix covers D5's core
// promise: a failure partway through the upload loop must remove every
// object already written, so 'stage' never sees a half-model.
func TestRunPullWeights_MidStreamFailureCleansUpPartialPrefix(t *testing.T) {
	files := []fakeHFFile{
		{"config.json", "{}"},
		{"tokenizer_config.json", "{}"},
		{"tokenizer.json", "{}"},
		{"model.safetensors", "weights"},
	}
	srv := newFakeHFServer(t, testHFRepo, "main", testHFSHA, files)
	hf := &hfhub.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	store := &failAfterNPutsStore{MemoryObjectStore: objectstore.NewMemoryObjectStore(), n: 2}

	_, err := runPullWeights(context.Background(), hf, store, selfHostModelID, testHFRepo, "main", true, "")
	require.Error(t, err)
	assert.Equal(t, 0, store.Count(), "objects uploaded before the failure must be cleaned up")
}

// TestRunPullWeights_DefaultsRepoFromCatalog covers the catalog repo default:
// with no --hf-repo, pull uses the self-host entry's canonical HFRepo rather
// than refusing, so an operator need not restate a repo the catalog knows.
func TestRunPullWeights_DefaultsRepoFromCatalog(t *testing.T) {
	files := []fakeHFFile{{"config.json", "{}"}, {"tokenizer_config.json", "{}"}, {"tokenizer.json", "{}"}, {"model.safetensors", "x"}}
	srv := newFakeHFServer(t, testHFRepo, "main", testHFSHA, files)
	hf := &hfhub.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	store := objectstore.NewMemoryObjectStore()

	finalURI, err := runPullWeights(context.Background(), hf, store, selfHostModelID, "", "main", false, "")
	require.NoError(t, err)
	assert.Equal(t, defaultPullPrefix(testHFRepo, testHFSHA), finalURI)
}

// TestRunPullWeights_DefaultsRevisionFromCatalogPin covers the pinned-revision
// default: a bare pull of an entry carrying HFRevision resolves that pin, not
// the "main" flag default, so a regressed upstream main cannot be picked up.
func TestRunPullWeights_DefaultsRevisionFromCatalogPin(t *testing.T) {
	const nomicModelID = "nomic-embed-text-v1.5"
	const nomicRepo = "nomic-ai/nomic-embed-text-v1.5"
	const pinnedRevision = "e5cf08a"
	files := []fakeHFFile{{"config.json", "{}"}, {"tokenizer_config.json", "{}"}, {"tokenizer.json", "{}"}, {"model.safetensors", "x"}}
	srv := newFakeHFServer(t, nomicRepo, pinnedRevision, testHFSHA, files)
	hf := &hfhub.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	store := objectstore.NewMemoryObjectStore()

	finalURI, err := runPullWeights(context.Background(), hf, store, nomicModelID, "", "main", false, "")
	require.NoError(t, err)
	assert.Equal(t, defaultPullPrefix(nomicRepo, testHFSHA), finalURI)
}

// TestPullFilesToPrefix_RefusesDuplicateFlattenedKey covers the flatten guard:
// two source paths sharing a basename would collide at one destination key, so
// the pull aborts rather than silently overwriting one with the other.
func TestPullFilesToPrefix_RefusesDuplicateFlattenedKey(t *testing.T) {
	files := []fakeHFFile{{"a/tokenizer.model", "first"}, {"b/tokenizer.model", "second"}}
	srv := newFakeHFServer(t, testHFRepo, "main", testHFSHA, files)
	hf := &hfhub.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	store := objectstore.NewMemoryObjectStore()

	entries := []hfhub.TreeEntry{{Type: "file", Path: "a/tokenizer.model"}, {Type: "file", Path: "b/tokenizer.model"}}
	_, err := pullFilesToPrefix(context.Background(), hf, store, testHFRepo, testHFSHA, "ochre-weights", "prefix/", entries)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "flatten to it")
}

// --- resolveHFToken (D2) ---

func newOchreWeightsPullTestCmd(t *testing.T, modelID, hfRepo, revision, s3URI, hfToken string) *cobra.Command {
	t.Helper()
	require.NoError(t, ochreWeightsPullCmd.Flags().Set("model-id", modelID))
	require.NoError(t, ochreWeightsPullCmd.Flags().Set("hf-repo", hfRepo))
	require.NoError(t, ochreWeightsPullCmd.Flags().Set("revision", revision))
	require.NoError(t, ochreWeightsPullCmd.Flags().Set("s3-uri", s3URI))
	require.NoError(t, ochreWeightsPullCmd.Flags().Set("hf-token", hfToken))
	return ochreWeightsPullCmd
}

// TestResolveHFToken_PrefersFlagOverEnvAndStored covers the top of D2's
// resolution order.
func TestResolveHFToken_PrefersFlagOverEnvAndStored(t *testing.T) {
	t.Setenv("HF_TOKEN", "hf_env_token")
	cmd := newOchreWeightsPullTestCmd(t, selfHostModelID, testHFRepo, "main", "", "hf_flag_token")
	cfg := &config.ClusterConfig{Node: "node1", Nodes: map[string]config.Config{"node1": {BaseDir: t.TempDir()}}}

	got := resolveHFToken(context.Background(), cmd, cfg, nil)
	assert.Equal(t, "hf_flag_token", got)
}

// TestResolveHFToken_FallsBackToEnv covers the second link: no flag, HF_TOKEN set.
func TestResolveHFToken_FallsBackToEnv(t *testing.T) {
	t.Setenv("HF_TOKEN", "hf_env_token")
	cmd := newOchreWeightsPullTestCmd(t, selfHostModelID, testHFRepo, "main", "", "")
	cfg := &config.ClusterConfig{Node: "node1", Nodes: map[string]config.Config{"node1": {BaseDir: t.TempDir()}}}

	got := resolveHFToken(context.Background(), cmd, cfg, nil)
	assert.Equal(t, "hf_env_token", got)
}

// TestResolveHFToken_NoTokenAnywhereReturnsEmpty covers the terminal case:
// no flag, no env, and no master key on disk to even attempt the stored
// credential -- must return empty, not error or panic, so a public repo
// pull still works standalone.
func TestResolveHFToken_NoTokenAnywhereReturnsEmpty(t *testing.T) {
	t.Setenv("HF_TOKEN", "")
	cmd := newOchreWeightsPullTestCmd(t, selfHostModelID, testHFRepo, "main", "", "")
	cfg := &config.ClusterConfig{Node: "node1", Nodes: map[string]config.Config{"node1": {BaseDir: t.TempDir()}}}

	got := resolveHFToken(context.Background(), cmd, cfg, nil)
	assert.Empty(t, got)
}

// TestResolveHFToken_FallsBackToStoredPlatformCredential covers D2's
// third link: with no flag or env, a credential stored via 'ochre
// credentials set' (platform account, vendor "huggingface") is used.
func TestResolveHFToken_FallsBackToStoredPlatformCredential(t *testing.T) {
	t.Setenv("HF_TOKEN", "")
	_, nc, js := testutil.StartTestJetStream(t)

	baseDir := t.TempDir()
	masterKey := bytes32Key()
	require.NoError(t, os.MkdirAll(filepath.Join(baseDir, "config"), 0750))
	require.NoError(t, handlers_iam.SaveMasterKey(filepath.Join(baseDir, "config", "master.key"), masterKey))

	credStore := gateway_bedrock.NewCredentialStore(js, masterKey, 1, nil)
	require.NoError(t, credStore.PutCredential(context.Background(), utils.GlobalAccountID, vendorHuggingFace, "hf_stored_token"))

	cmd := newOchreWeightsPullTestCmd(t, selfHostModelID, testHFRepo, "main", "", "")
	cfg := &config.ClusterConfig{Node: "node1", Nodes: map[string]config.Config{"node1": {BaseDir: baseDir}}}

	got := resolveHFToken(context.Background(), cmd, cfg, nc)
	assert.Equal(t, "hf_stored_token", got)
}

func bytes32Key() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

func strPtr(s string) *string { return &s }

// --- runOchreWeightsPull wrapper ---

// TestRunOchreWeightsPull_ConnectFailureExits1 mirrors stage's own coverage:
// pull cannot even reach the catalog check without a cluster connection.
func TestRunOchreWeightsPull_ConnectFailureExits1(t *testing.T) {
	stubConnect(t, nil, nil, errors.New("dial failed"))

	cmd := newOchreWeightsPullTestCmd(t, selfHostModelID, testHFRepo, "main", "", "")
	code := withOchreExitCapture(t, func() { runOchreWeightsPull(cmd, nil) })
	assert.Equal(t, 1, code)
}

// TestRunOchreWeightsPull_UnknownModelExits1 drives the real cobra Run
// function end to end, confirming the wrapper propagates runPullWeights's
// catalog refusal into a process exit.
func TestRunOchreWeightsPull_UnknownModelExits1(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	stubConnect(t, fakeClusterConfig(), nc, nil)

	cmd := newOchreWeightsPullTestCmd(t, "not-a-real-model", testHFRepo, "main", "", "")
	code := withOchreExitCapture(t, func() { runOchreWeightsPull(cmd, nil) })
	assert.Equal(t, 1, code)
}

// --- ochre credentials set ---

// withStdin swaps os.Stdin for a pipe fed with content, restoring the real
// os.Stdin afterwards, mirroring captureStdout's os.Stdout swap.
func withStdin(t *testing.T, content string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stdin
	os.Stdin = r                          //nolint:reassign // test-local stdin swap, restored below
	t.Cleanup(func() { os.Stdin = orig }) //nolint:reassign // restoring the real os.Stdin captured above

	go func() {
		_, _ = io.WriteString(w, content)
		_ = w.Close()
	}()

	fn()
}

func newOchreCredentialsSetTestCmd(t *testing.T, vendor, account, token string) *cobra.Command {
	t.Helper()
	require.NoError(t, ochreCredentialsSetCmd.Flags().Set("vendor", vendor))
	require.NoError(t, ochreCredentialsSetCmd.Flags().Set("account", account))
	require.NoError(t, ochreCredentialsSetCmd.Flags().Set("token", token))
	return ochreCredentialsSetCmd
}

// TestReadTokenFromStdin_TrimsTrailingNewline covers the common 'echo token
// | cmd --token -' shape.
func TestReadTokenFromStdin_TrimsTrailingNewline(t *testing.T) {
	token, err := readTokenFromStdin(strings.NewReader("hf_mytoken\n"))
	require.NoError(t, err)
	assert.Equal(t, "hf_mytoken", token)
}

// TestReadTokenFromStdin_EmptyIsError covers a piped-in nothing (e.g. an
// empty file) refusing rather than silently storing an empty credential.
func TestReadTokenFromStdin_EmptyIsError(t *testing.T) {
	_, err := readTokenFromStdin(strings.NewReader(""))
	require.Error(t, err)
}

// TestRunOchreCredentialsSet_RejectsNonDashToken covers the no-secrets-as-args
// guard: --token must be literally '-', never the secret itself.
func TestRunOchreCredentialsSet_RejectsNonDashToken(t *testing.T) {
	cmd := newOchreCredentialsSetTestCmd(t, vendorHuggingFace, "", "hf_literal_secret")
	code := withOchreExitCapture(t, func() { runOchreCredentialsSet(cmd, nil) })
	assert.Equal(t, 1, code)
}

// TestRunOchreCredentialsSet_ConnectFailureExits1 covers the connect
// failure path, same shape as every other ochre wrapper.
func TestRunOchreCredentialsSet_ConnectFailureExits1(t *testing.T) {
	stubConnect(t, nil, nil, errors.New("dial failed"))

	cmd := newOchreCredentialsSetTestCmd(t, vendorHuggingFace, "", "-")
	withStdin(t, "hf_mytoken\n", func() {
		code := withOchreExitCapture(t, func() { runOchreCredentialsSet(cmd, nil) })
		assert.Equal(t, 1, code)
	})
}

// TestRunOchreCredentialsSet_MasterKeyLoadFailureExits1 covers the store's
// own setup failing (no master.key provisioned yet) before PutCredential is
// ever reached.
func TestRunOchreCredentialsSet_MasterKeyLoadFailureExits1(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	cfg := &config.ClusterConfig{Node: "node1", Nodes: map[string]config.Config{"node1": {BaseDir: t.TempDir()}}}
	stubConnect(t, cfg, nc, nil)

	cmd := newOchreCredentialsSetTestCmd(t, vendorHuggingFace, "", "-")
	withStdin(t, "hf_mytoken\n", func() {
		code := withOchreExitCapture(t, func() { runOchreCredentialsSet(cmd, nil) })
		assert.Equal(t, 1, code)
	})
}

// TestRunOchreCredentialsSet_RoundTripsAndDefaultsToPlatformAccount drives
// the real Run function end to end: stores a credential read from stdin
// under the platform account (D2's default), then confirms a separately
// constructed CredentialStore resolves it, and that the token never appears
// on stdout.
func TestRunOchreCredentialsSet_RoundTripsAndDefaultsToPlatformAccount(t *testing.T) {
	ns, nc, _ := testutil.StartTestJetStream(t)
	baseDir := t.TempDir()
	masterKey := bytes32Key()
	require.NoError(t, os.MkdirAll(filepath.Join(baseDir, "config"), 0750))
	require.NoError(t, handlers_iam.SaveMasterKey(filepath.Join(baseDir, "config", "master.key"), masterKey))
	cfg := &config.ClusterConfig{Node: "node1", Nodes: map[string]config.Config{"node1": {BaseDir: baseDir}}}
	stubConnect(t, cfg, nc, nil)

	cmd := newOchreCredentialsSetTestCmd(t, vendorHuggingFace, "", "-")
	var out string
	withStdin(t, "hf_supersecret\n", func() {
		out = captureStdout(t, func() { runOchreCredentialsSet(cmd, nil) })
	})

	assert.NotContains(t, out, "hf_supersecret", "the token must never be printed")

	// runOchreCredentialsSet closes its own NATS connection on return, so
	// verification dials a second connection to the same embedded server.
	verifyConn, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer verifyConn.Close()
	verifyJS := testutil.NewJetStream(t, verifyConn)

	credStore := gateway_bedrock.NewCredentialStore(verifyJS, masterKey, 1, nil)
	token, ok, err := credStore.Resolve(context.Background(), utils.GlobalAccountID, vendorHuggingFace)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "hf_supersecret", token)
}
