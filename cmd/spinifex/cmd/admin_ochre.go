package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/bluebottle/pkg/masterkey"
	"github.com/mulgadc/bluebottle/pkg/safecast"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	"github.com/mulgadc/spinifex/spinifex/gateway/bedrock/hfhub"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
)

var ochreCmd = &cobra.Command{
	Use:   "ochre",
	Short: "Manage Ochre (self-hosted model inference) resources",
}

var ochreWeightsCmd = &cobra.Command{
	Use:   "weights",
	Short: "Stage, list and remove self-host model weights",
	Long: `Give an operator a supported path to stage a self-hosted model's weights so
Ochre can advertise and serve it. A model with no staged weights is hidden
from ListFoundationModels rather than advertised and broken.`,
}

var ochreWeightsStageCmd = &cobra.Command{
	Use:   "stage",
	Short: "Materialise a self-host model's weights from predastore into a servable snapshot",
	Long: `stage takes an S3 URI pointing at a Hugging Face model directory already
uploaded to predastore (e.g. via 'aws s3 cp --recursive'), verifies the
required files are present, materialises them into a viperblock volume,
snapshots it, and records the source URI and snapshot ID against --model-id
in the bedrock-weights KV bucket.

Idempotent: re-staging the same --s3-uri for a model that already has it
staged is a no-op. Re-staging a different --s3-uri replaces the KV entry and
reports the previous snapshot ID so an operator can reclaim it separately.

Refuses before materialising anything if --model-id is not a self-host
catalog entry, or if the S3 prefix is missing any required file.`,
	Run: runOchreWeightsStage,
}

var ochreWeightsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List staged self-host model weights",
	Run:   runOchreWeightsList,
}

var ochreWeightsRemoveCmd = &cobra.Command{
	Use:   "remove",
	Short: "Drop a model's staged-weights KV entry",
	Long: `remove drops --model-id's entry from the bedrock-weights KV bucket, which
hides it from ListFoundationModels again. It never deletes the underlying
snapshot or the source S3 objects; reclaiming that storage is a separate,
explicit act.`,
	Run: runOchreWeightsRemove,
}

var ochreWeightsPullCmd = &cobra.Command{
	Use:   "pull",
	Short: "Fetch a self-host model's weights from Hugging Face into predastore",
	Long: `pull resolves --revision to an immutable commit SHA, lists the repo's file
tree, downloads only *.safetensors shards plus the fixed config/tokenizer
set, and streams each into predastore under --s3-uri (or a keyed default
when omitted). It never touches the offline 'stage' path or the weights KV
store itself; it prints the resulting s3:// URI for 'weights stage --s3-uri'.

Refuses before any object lands if --model-id is not a self-host catalog
entry, the repo has no safetensors, or the hub returns 401/403 (a gated repo
with no usable token). A failure partway through removes every object
already written so 'stage' never sees a half-model.`,
	Run: runOchreWeightsPull,
}

// ochreCredentialsCmd groups vendor credential management for pull's
// optional stored-token fallback (D2); it holds no Run of its own.
var ochreCredentialsCmd = &cobra.Command{
	Use:   "credentials",
	Short: "Manage stored provider credentials for self-host model pulls",
}

var ochreCredentialsSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Store a vendor API token/licence credential",
	Long: `set stores --vendor's token, encrypted at rest under the cluster's IAM master
key, in the bedrock-credentials KV bucket. --account defaults to the
platform account, since self-host models are platform-level, not per-tenant.

The token itself is never a command-line argument: --token must be '-', and
the secret is read from stdin, so it never appears in shell history or a
process listing.`,
	Run: runOchreCredentialsSet,
}

func init() {
	adminCmd.AddCommand(ochreCmd)
	ochreCmd.AddCommand(ochreWeightsCmd)
	ochreCmd.AddCommand(ochreCredentialsCmd)
	ochreWeightsCmd.AddCommand(ochreWeightsStageCmd)
	ochreWeightsCmd.AddCommand(ochreWeightsListCmd)
	ochreWeightsCmd.AddCommand(ochreWeightsRemoveCmd)
	ochreWeightsCmd.AddCommand(ochreWeightsPullCmd)
	ochreCredentialsCmd.AddCommand(ochreCredentialsSetCmd)

	ochreWeightsStageCmd.Flags().String("model-id", "", "Catalog model ID to stage weights for (required)")
	ochreWeightsStageCmd.Flags().String("s3-uri", "", "predastore S3 URI holding the model's Hugging Face files, e.g. s3://bucket/prefix/ (required)")
	ochreWeightsStageCmd.Flags().String("tmp-dir", os.TempDir(), "Temporary directory for download and volume staging")
	_ = ochreWeightsStageCmd.MarkFlagRequired("model-id")
	_ = ochreWeightsStageCmd.MarkFlagRequired("s3-uri")

	ochreWeightsRemoveCmd.Flags().String("model-id", "", "Model ID to remove from staged weights (required)")
	_ = ochreWeightsRemoveCmd.MarkFlagRequired("model-id")

	ochreWeightsPullCmd.Flags().String("model-id", "", "Self-host catalog model ID this pull is for (required)")
	ochreWeightsPullCmd.Flags().String("hf-repo", "", "Hugging Face repo, e.g. meta-llama/Llama-3.2-1B-Instruct (default: the catalog entry's repo)")
	ochreWeightsPullCmd.Flags().String("revision", "main", "Hugging Face branch, tag or commit SHA to resolve and pin (default: the catalog entry's pinned revision)")
	ochreWeightsPullCmd.Flags().String("s3-uri", "", "predastore destination prefix, e.g. s3://bucket/prefix/ (default: s3://ochre-weights/<repo>/<sha>/)")
	ochreWeightsPullCmd.Flags().String("hf-token", "", "Hugging Face token for a gated repo (falls back to HF_TOKEN, then a stored platform credential)")
	_ = ochreWeightsPullCmd.MarkFlagRequired("model-id")

	ochreCredentialsSetCmd.Flags().String("vendor", "", "Vendor to store a credential for, e.g. huggingface (required)")
	ochreCredentialsSetCmd.Flags().String("account", "", "Account ID to store the credential under (default: platform account)")
	ochreCredentialsSetCmd.Flags().String("token", "", "Must be '-': reads the secret from stdin (required)")
	_ = ochreCredentialsSetCmd.MarkFlagRequired("vendor")
	_ = ochreCredentialsSetCmd.MarkFlagRequired("token")
}

// requiredWeightsFiles are the fixed-name Hugging Face artefacts stage
// refuses to materialise without, mirroring what AWS Bedrock's
// CreateModelImportJob expects at its S3 source prefix. At least one
// *.safetensors file and one tokenizer file are checked separately.
var requiredWeightsFiles = []string{
	"config.json",
	"tokenizer_config.json",
}

// tokenizerFileNames are the two shapes a Hugging Face repo ships its
// tokenizer under: modern repos ship only tokenizer.json (fast tokenizer),
// older ones only tokenizer.model (SentencePiece) -- e.g. Llama 3.x keeps
// tokenizer.model under original/, not at the prefix root. Either is enough.
var tokenizerFileNames = []string{"tokenizer.json", "tokenizer.model"}

// vendorHuggingFace is the CredentialStore vendor key 'ochre credentials
// set' and pull's stored-credential fallback share for the Hugging Face
// licence token (D2).
const vendorHuggingFace = "huggingface"

// ochrePullManifestFile is the fixed name 'weights pull' writes into the
// destination prefix and 'weights stage' reads back (D3), recording which
// upstream commit a staged model's weights came from.
const ochrePullManifestFile = "ochre-pull.json"

// ochrePullManifest is the JSON body of ochrePullManifestFile.
type ochrePullManifest struct {
	HFRepo      string    `json:"hf_repo"`
	RevisionSHA string    `json:"revision_sha"`
	PulledAt    time.Time `json:"pulled_at"`
}

// rootOnlyPullFileNames are the fixed-name config artefacts pull fetches only
// at the repo root. A same-named file in a subdir is a component config, not
// the model's -- e.g. sentence-transformers' 1_Pooling/config.json -- and must
// never flatten onto the root config.json, so nested matches are dropped.
var rootOnlyPullFileNames = map[string]bool{
	"config.json":                  true,
	"tokenizer_config.json":        true,
	"generation_config.json":       true,
	"special_tokens_map.json":      true,
	"model.safetensors.index.json": true,
}

// nestablePullFileNames are the tokenizer artefacts matched by basename at any
// depth: some repos keep them in a subdir (e.g. Llama's original/tokenizer.model).
var nestablePullFileNames = map[string]bool{
	"tokenizer.json":  true,
	"tokenizer.model": true,
}

// selectPullFiles filters a Hugging Face tree listing down to the
// safetensors-only set pull downloads (D4): every *.safetensors file plus
// the fixed config/tokenizer set. Anything else -- notably .bin/.pt pickle
// checkpoints, which can execute code on load -- is dropped silently rather
// than aborting the whole pull.
func selectPullFiles(entries []hfhub.TreeEntry) []hfhub.TreeEntry {
	var selected []hfhub.TreeEntry
	for _, e := range entries {
		if e.Type != "file" {
			continue
		}
		base := path.Base(e.Path)
		nested := strings.Contains(e.Path, "/")
		switch {
		case strings.HasSuffix(e.Path, ".safetensors"):
			selected = append(selected, e)
		case nestablePullFileNames[base]:
			selected = append(selected, e)
		case rootOnlyPullFileNames[base] && !nested:
			selected = append(selected, e)
		}
	}
	return selected
}

// anySafetensors reports whether entries contains at least one *.safetensors
// file. selectPullFiles's config/tokenizer set alone is never enough --
// D4 aborts a pull with no actual weights to serve.
func anySafetensors(entries []hfhub.TreeEntry) bool {
	for _, e := range entries {
		if strings.HasSuffix(e.Path, ".safetensors") {
			return true
		}
	}
	return false
}

// defaultPullPrefix derives D7's fallback destination when --s3-uri is
// omitted: keyed by the exact repo and resolved commit SHA, so re-pulling
// the same commit lands at the same, self-deduping URI.
func defaultPullPrefix(hfRepo, sha string) string {
	return fmt.Sprintf("s3://ochre-weights/%s/%s/", strings.Trim(hfRepo, "/"), sha)
}

// putPullManifest writes ochrePullManifestFile into bucket/prefix, the last
// step of a successful pull (D3).
func putPullManifest(ctx context.Context, store objectstore.ObjectStore, bucket, prefix string, manifest ochrePullManifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode pull manifest: %w", err)
	}
	key := prefix + ochrePullManifestFile
	if _, err := store.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data)}); err != nil {
		return fmt.Errorf("put pull manifest s3://%s/%s: %w", bucket, key, err)
	}
	return nil
}

// readPullManifest reads and decodes bucket/prefix's ochre-pull.json, if
// present. A missing manifest is not an error: stage's offline path (an
// operator-supplied prefix with no pull manifest) must keep working
// unchanged, so ok is simply false.
func readPullManifest(ctx context.Context, store objectstore.ObjectStore, bucket, prefix string) (ochrePullManifest, bool, error) {
	key := prefix + ochrePullManifestFile
	out, err := store.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return ochrePullManifest{}, false, nil
		}
		return ochrePullManifest{}, false, fmt.Errorf("get pull manifest s3://%s/%s: %w", bucket, key, err)
	}
	defer out.Body.Close()

	var manifest ochrePullManifest
	if err := json.NewDecoder(out.Body).Decode(&manifest); err != nil {
		return ochrePullManifest{}, false, fmt.Errorf("decode pull manifest s3://%s/%s: %w", bucket, key, err)
	}
	return manifest, true, nil
}

// cleanupPulledObjects best-effort deletes every key already written by a
// pull that failed partway through (D5): a half-uploaded prefix must never
// be left for 'stage' to mistake for a complete model. A delete failure is
// logged, not returned -- the original pull error is what the operator needs.
func cleanupPulledObjects(ctx context.Context, store objectstore.ObjectStore, bucket string, keys []string) {
	for _, key := range keys {
		if _, err := store.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not clean up s3://%s/%s after a failed pull: %v\n", bucket, key, err)
		}
	}
}

// pullOneFile downloads hfPath from the hub to a local temp file and PUTs it
// to bucket/key. A temp file is required, not a direct HTTP-to-S3 pipe:
// predastore's PutObject signs the request over an io.ReadSeeker, which an
// HTTP response body is not. tmpDir is cleaned up by the caller.
func pullOneFile(ctx context.Context, hf *hfhub.Client, store objectstore.ObjectStore, hfRepo, sha, hfPath, bucket, key, tmpDir string) error {
	body, _, err := hf.DownloadFile(ctx, hfRepo, sha, hfPath)
	if err != nil {
		return err
	}
	defer body.Close()

	tmpFile, err := os.CreateTemp(tmpDir, "part-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	if _, err := io.Copy(tmpFile, body); err != nil {
		return fmt.Errorf("download to temp file: %w", err)
	}
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek temp file: %w", err)
	}

	if _, err := store.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: tmpFile}); err != nil {
		return fmt.Errorf("put s3://%s/%s: %w", bucket, key, err)
	}
	return nil
}

// pullFilesToPrefix downloads and uploads every selected file, one at a time
// so at most one file is ever held on local disk (D6). Keys are flattened to
// each file's basename to match the flat layout 'stage' validates -- e.g.
// Llama's original/tokenizer.model lands at <prefix>tokenizer.model. It
// returns the keys written so far even on error, so the caller can clean up
// a partial prefix (D5).
func pullFilesToPrefix(ctx context.Context, hf *hfhub.Client, store objectstore.ObjectStore, hfRepo, sha, bucket, prefix string, files []hfhub.TreeEntry) ([]string, error) {
	tmpDir, err := os.MkdirTemp("", "spinifex-ochre-pull-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	var uploaded []string
	seen := make(map[string]string, len(files))
	for _, f := range files {
		key := prefix + path.Base(f.Path)
		if prev, dup := seen[key]; dup {
			return uploaded, fmt.Errorf("refusing to overwrite %s: both %s and %s flatten to it", key, prev, f.Path)
		}
		seen[key] = f.Path
		if err := pullOneFile(ctx, hf, store, hfRepo, sha, f.Path, bucket, key, tmpDir); err != nil {
			return uploaded, fmt.Errorf("pull %s: %w", f.Path, err)
		}
		uploaded = append(uploaded, key)
	}
	return uploaded, nil
}

// weightsStagingRefusal builds the model-ID refusal error weights staging and
// pulling share. Unknown IDs and provider-served IDs get distinct wording so
// a caller can tell which one it hit; taking found/selfHost as plain bools
// keeps this testable without a real catalog entry for the provider case.
func weightsStagingRefusal(modelID, operation string, found, _ bool) error {
	if !found {
		return fmt.Errorf("unknown model ID %q: not present in the Ochre catalog", modelID)
	}
	return fmt.Errorf("%q is a provider-served model, not self-host; weights %s does not apply", modelID, operation)
}

// runPullWeights holds 'ochre weights pull' decision and side-effect logic:
// catalog validation, ref resolution to an immutable commit SHA (D3), tree
// listing and safetensors-only filtering (D4), streaming each selected file
// into predastore (D6), and finally the ochre-pull.json manifest. It never
// touches the weights KV store; 'stage' consumes the printed s3:// URI
// separately. A failure removes every object already written (D5).
func runPullWeights(ctx context.Context, hf *hfhub.Client, store objectstore.ObjectStore, modelID, hfRepo, revision string, revisionExplicit bool, s3URIFlag string) (string, error) {
	spec, found, selfHost := gateway_bedrock.LookupServingSpec(modelID)
	if !found || !selfHost {
		return "", weightsStagingRefusal(modelID, "pull", found, selfHost)
	}

	// The catalog carries the canonical repo and a pinned revision so a bare
	// pull cannot silently grab a regressed upstream default; an explicit flag
	// still overrides either.
	if hfRepo == "" {
		hfRepo = spec.HFRepo
	}
	if hfRepo == "" {
		return "", fmt.Errorf("no --hf-repo given and %q has no catalog HF repo", modelID)
	}
	if !revisionExplicit && spec.HFRevision != "" {
		revision = spec.HFRevision
	}

	fmt.Printf("Resolving %s@%s ...\n", hfRepo, revision)
	sha, err := hf.ResolveRevision(ctx, hfRepo, revision)
	if err != nil {
		return "", err
	}

	fmt.Printf("Listing files at %s@%s ...\n", hfRepo, sha)
	tree, err := hf.ListTree(ctx, hfRepo, sha)
	if err != nil {
		return "", err
	}

	selected := selectPullFiles(tree)
	if !anySafetensors(selected) {
		return "", fmt.Errorf("%s@%s has no *.safetensors files; refusing to pull pickle-format (.bin) weights", hfRepo, sha)
	}

	s3URI := s3URIFlag
	if s3URI == "" {
		s3URI = defaultPullPrefix(hfRepo, sha)
	}
	bucket, prefix, err := parseWeightsS3URI(s3URI)
	if err != nil {
		return "", err
	}

	fmt.Printf("Pulling %d file(s) into s3://%s/%s ...\n", len(selected), bucket, prefix)
	uploaded, err := pullFilesToPrefix(ctx, hf, store, hfRepo, sha, bucket, prefix, selected)
	if err != nil {
		cleanupPulledObjects(ctx, store, bucket, uploaded)
		return "", err
	}

	manifest := ochrePullManifest{HFRepo: hfRepo, RevisionSHA: sha, PulledAt: time.Now().UTC()}
	if err := putPullManifest(ctx, store, bucket, prefix, manifest); err != nil {
		cleanupPulledObjects(ctx, store, bucket, uploaded)
		return "", err
	}

	return fmt.Sprintf("s3://%s/%s", bucket, prefix), nil
}

// parseWeightsS3URI splits an s3://bucket/prefix URI into its bucket and
// prefix. The prefix is normalised to end with '/' so downstream listing and
// validation scope to the directory rather than any key merely sharing the
// prefix as a substring.
func parseWeightsS3URI(uri string) (bucket, prefix string, err error) {
	trimmed := strings.TrimPrefix(uri, "s3://")
	if trimmed == uri {
		return "", "", fmt.Errorf("invalid --s3-uri %q: expected s3://bucket/prefix", uri)
	}
	parts := strings.SplitN(trimmed, "/", 2)
	bucket = parts[0]
	if bucket == "" {
		return "", "", fmt.Errorf("invalid --s3-uri %q: missing bucket", uri)
	}
	if len(parts) == 2 {
		prefix = parts[1]
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return bucket, prefix, nil
}

// listWeightsPrefix pages through every object under bucket/prefix, calling
// fn for each. Both validateWeightsPrefix (presence-only) and
// downloadWeightsPrefix (full copy) share this walk.
func listWeightsPrefix(ctx context.Context, store objectstore.ObjectStore, bucket, prefix string, fn func(*s3.Object) error) error {
	var token *string
	for {
		out, err := store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return err
		}
		for _, obj := range out.Contents {
			if err := fn(obj); err != nil {
				return err
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated || out.NextContinuationToken == nil {
			return nil
		}
		token = out.NextContinuationToken
	}
}

// validateWeightsPrefix confirms every required Hugging Face file exists
// under bucket/prefix before stage downloads anything, so a typo'd prefix
// fails in milliseconds rather than after materialising multiple gigabytes.
func validateWeightsPrefix(ctx context.Context, store objectstore.ObjectStore, bucket, prefix string) error {
	var missing []string
	for _, name := range requiredWeightsFiles {
		key := prefix + name
		if _, err := store.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); err != nil {
			if objectstore.IsNoSuchKeyError(err) {
				missing = append(missing, name)
				continue
			}
			return fmt.Errorf("head s3://%s/%s: %w", bucket, key, err)
		}
	}

	hasSafetensors := false
	if err := listWeightsPrefix(ctx, store, bucket, prefix, func(obj *s3.Object) error {
		if strings.HasSuffix(aws.StringValue(obj.Key), ".safetensors") {
			hasSafetensors = true
		}
		return nil
	}); err != nil {
		return fmt.Errorf("list s3://%s/%s: %w", bucket, prefix, err)
	}
	if !hasSafetensors {
		missing = append(missing, "*.safetensors")
	}

	hasTokenizer := false
	for _, name := range tokenizerFileNames {
		key := prefix + name
		if _, err := store.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); err == nil {
			hasTokenizer = true
			break
		} else if !objectstore.IsNoSuchKeyError(err) {
			return fmt.Errorf("head s3://%s/%s: %w", bucket, key, err)
		}
	}
	if !hasTokenizer {
		missing = append(missing, "tokenizer.json or tokenizer.model")
	}

	if len(missing) > 0 {
		return fmt.Errorf("s3://%s/%s is missing required file(s): %s", bucket, prefix, strings.Join(missing, ", "))
	}
	return nil
}

// downloadWeightsPrefix copies every object under bucket/prefix into
// destDir, flattening predastore's key structure to the basename: a Hugging
// Face model directory is flat, and the filesystem image stage builds only
// needs the files, not the source key layout.
func downloadWeightsPrefix(ctx context.Context, store objectstore.ObjectStore, bucket, prefix, destDir string) (int64, error) {
	var total int64
	err := listWeightsPrefix(ctx, store, bucket, prefix, func(obj *s3.Object) error {
		key := aws.StringValue(obj.Key)
		if strings.HasSuffix(key, "/") {
			return nil // directory marker, not a file
		}
		n, err := downloadObjectTo(ctx, store, bucket, key, filepath.Join(destDir, path.Base(key)))
		if err != nil {
			return fmt.Errorf("download s3://%s/%s: %w", bucket, key, err)
		}
		total += n
		return nil
	})
	return total, err
}

func downloadObjectTo(ctx context.Context, store objectstore.ObjectStore, bucket, key, destPath string) (int64, error) {
	out, err := store.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return 0, err
	}
	defer out.Body.Close()

	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	return io.Copy(f, out.Body)
}

// mkfsExt4SbinDirs are searched when mkfs.ext4 is absent from PATH. e2fsprogs
// installs into sbin, which a non-login shell routinely omits, so a plain
// LookPath fails for an unprivileged operator on an otherwise fine host.
var mkfsExt4SbinDirs = []string{"/usr/local/sbin", "/usr/sbin", "/sbin"}

// lookupMkfsExt4 resolves mkfs.ext4 from PATH, falling back to the sbin dirs.
func lookupMkfsExt4() (string, error) {
	if path, err := exec.LookPath("mkfs.ext4"); err == nil {
		return path, nil
	}
	for _, dir := range mkfsExt4SbinDirs {
		candidate := filepath.Join(dir, "mkfs.ext4")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("mkfs.ext4 not found on PATH or in %s (install e2fsprogs)",
		strings.Join(mkfsExt4SbinDirs, ", "))
}

// mkfsExt4Runner populates imagePath as an ext4 filesystem from srcDir's
// files, normally by shelling out to mkfs.ext4 -d. A package var, mirroring
// caBakeRunner, so tests can substitute a fake instead of requiring
// mkfs.ext4 on PATH.
var mkfsExt4Runner = func(srcDir, imagePath string) error {
	mkfs, err := lookupMkfsExt4()
	if err != nil {
		return err
	}
	out, err := exec.Command(mkfs, "-F", "-d", srcDir, imagePath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4: %w: %s", err, string(out))
	}
	return nil
}

// buildWeightsImage packages srcDir's files into a raw ext4 filesystem image
// at imagePath, sized to fit their total bytes plus filesystem overhead and
// headroom. mkfs.ext4 -d populates the filesystem directly from srcDir, so
// no loopback mount (and no root) is needed -- unlike the guestfish/
// virt-customize tooling build-system-image.sh needs to customize a
// bootable cloud image, a weights volume is just a directory of files.
func buildWeightsImage(srcDir, imagePath string, contentBytes int64) error {
	const overheadFraction = 0.15 // ext4 metadata + inode table headroom
	const minPaddingBytes = 64 * 1024 * 1024
	padding := max(int64(float64(contentBytes)*overheadFraction), minPaddingBytes)
	sizeBytes := contentBytes + padding

	f, err := os.OpenFile(imagePath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("create image file: %w", err)
	}
	if err := f.Truncate(sizeBytes); err != nil {
		f.Close()
		return fmt.Errorf("size image file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close image file: %w", err)
	}

	return mkfsExt4Runner(srcDir, imagePath)
}

// registerWeightsSnapshot writes the EC2 control plane's half of the snapshot
// prefix. CreateSnapshot above writes only viperblock's half -- the block
// checkpoint and config.json -- but CreateVolume resolves a SnapshotId through
// metadata.json sitting alongside them, so without this the endpoint launcher
// cannot see the snapshot at all and fails with InvalidSnapshot.NotFound.
func registerWeightsSnapshot(store objectstore.ObjectStore, bucket, snapshotID, volumeID string, volumeSize uint64, az string, encrypted bool) error {
	cfg := ebsmetadata.Snapshot{
		SnapshotID: snapshotID,
		VolumeID:   volumeID,
		// GiB, not bytes: CreateVolume compares this against a requested Size
		// already in GiB, and would reject every clone if handed raw bytes.
		VolumeSize:       safecast.Uint64ToInt64(volumeSize / bytesPerGiB),
		State:            "completed",
		Progress:         "100%",
		StartTime:        time.Now(),
		Description:      fmt.Sprintf("Ochre weights volume %s", volumeID),
		Encrypted:        encrypted,
		OwnerID:          utils.GlobalAccountID,
		AvailabilityZone: az,
	}
	if err := ebsmetadata.NewStore(store, bucket).PutSnapshot(context.Background(), cfg); err != nil {
		return fmt.Errorf("register snapshot metadata: %w", err)
	}
	return nil
}

// weightsMaterializer builds a servable snapshot from downloadDir's already
// downloaded and validated Hugging Face files. Isolating this behind a
// function value lets runStageWeights be tested end to end -- including the
// idempotent-noop and replace-and-report-previous-snapshot decisions -- with
// a fake standing in for the real mkfs.ext4 + viperblock + predastore work,
// which needs a live environment (see materializeWeightsVolume).
type weightsMaterializer func(ctx context.Context, downloadDir string, contentBytes int64) (snapshotID string, err error)

// weightsSnapshotChecker reports whether snapshotID still exists in the
// snapshot store, so runStageWeights can tell a healthy KV record from one
// whose snapshot was lost to a host rebuild or volume GC.
type weightsSnapshotChecker func(ctx context.Context, snapshotID string) (bool, error)

// runStageWeights holds 'ochre weights stage' decision and side-effect logic:
// catalog validation, S3 URI parsing, idempotency/replacement detection
// against the existing KV record, prefix validation, download, materialisation
// via materialize, and the final KV write. It has no cobra or NATS-connection
// dependency of its own, so tests drive it directly against a fake object
// store, a real WeightsStore backed by an embedded JetStream server, and a
// fake materializer.
func runStageWeights(ctx context.Context, store objectstore.ObjectStore, weightsStore *gateway_bedrock.WeightsStore, tmpDirFlag, modelID, s3URI string, materialize weightsMaterializer, checkSnapshotLive weightsSnapshotChecker) (string, error) {
	if _, found, selfHost := gateway_bedrock.LookupServingSpec(modelID); !found || !selfHost {
		return "", weightsStagingRefusal(modelID, "staging", found, selfHost)
	}

	bucket, prefix, err := parseWeightsS3URI(s3URI)
	if err != nil {
		return "", err
	}
	sourceURI := fmt.Sprintf("s3://%s/%s", bucket, prefix)

	existing, hadPrevious, err := weightsStore.GetWeights(ctx, modelID)
	if err != nil {
		return "", err
	}
	var replacingMissingSnapshot bool
	if hadPrevious && existing.SourceURI == sourceURI {
		live, err := checkSnapshotLive(ctx, existing.SnapshotID)
		if err != nil {
			return "", err
		}
		if live {
			return fmt.Sprintf("%s is already staged from %s (snapshot %s); nothing to do.", modelID, sourceURI, existing.SnapshotID), nil
		}
		// The KV record is otherwise up to date; only its snapshot is gone, so
		// fall through and self-heal rather than making the operator run remove.
		replacingMissingSnapshot = true
	}

	fmt.Printf("Validating %s ...\n", sourceURI)
	if err := validateWeightsPrefix(ctx, store, bucket, prefix); err != nil {
		return "", err
	}

	// A pull manifest is optional: absent means an operator-supplied prefix
	// from the offline path, which stage has always supported and must keep
	// supporting unchanged.
	var sourceRevision string
	if manifest, ok, err := readPullManifest(ctx, store, bucket, prefix); err != nil {
		return "", err
	} else if ok {
		sourceRevision = manifest.RevisionSHA
	}

	tmpDir, err := os.MkdirTemp(tmpDirFlag, "spinifex-weights-tmp-*")
	if err != nil {
		return "", fmt.Errorf("could not create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	downloadDir := filepath.Join(tmpDir, "src")
	if err := os.MkdirAll(downloadDir, 0700); err != nil {
		return "", fmt.Errorf("could not create download dir: %w", err)
	}

	fmt.Printf("Downloading %s ...\n", sourceURI)
	contentBytes, err := downloadWeightsPrefix(ctx, store, bucket, prefix, downloadDir)
	if err != nil {
		return "", err
	}

	snapshotID, err := materialize(ctx, downloadDir, contentBytes)
	if err != nil {
		return "", err
	}

	if err := weightsStore.PutWeightsWithRevision(ctx, modelID, sourceURI, snapshotID, sourceRevision); err != nil {
		return "", err
	}

	if replacingMissingSnapshot {
		return fmt.Sprintf("✅ Staged %s from %s (snapshot %s). Replaced MISSING snapshot %s -- it was no longer present in the snapshot store.",
			modelID, sourceURI, snapshotID, existing.SnapshotID), nil
	}
	if hadPrevious {
		return fmt.Sprintf("✅ Staged %s from %s (snapshot %s). Replaced previous snapshot %s -- reclaim it separately if no longer needed.",
			modelID, sourceURI, snapshotID, existing.SnapshotID), nil
	}
	return fmt.Sprintf("✅ Staged %s from %s (snapshot %s).", modelID, sourceURI, snapshotID), nil
}

// materializeWeightsVolume is the real weightsMaterializer: it packages
// downloadDir into a filesystem image, imports it into a new volume through
// the EBS provider, and snapshots it. This is the expensive,
// environment-dependent half of stage -- it shells out to mkfs.ext4 and needs
// a live provider -- so it is exercised live rather than in unit tests.
func materializeWeightsVolume(ctx context.Context, provider ebsprovider.EBSProvider, nodeID string, node config.Config, downloadDir string, contentBytes int64) (string, error) {
	tmpDir := filepath.Dir(downloadDir)
	volumeId := utils.GenerateResourceID("vol")
	imagePath := filepath.Join(tmpDir, volumeId+".img")

	fmt.Println("Building filesystem image ...")
	if err := buildWeightsImage(downloadDir, imagePath, contentBytes); err != nil {
		return "", err
	}

	imageStat, err := os.Stat(imagePath)
	if err != nil {
		return "", fmt.Errorf("could not stat image: %w", err)
	}

	// Round the volume up to a whole GiB rather than using the raw image size,
	// so the byte size and the recorded SizeGiB describe the same volume.
	sizeGiB := amiVolumeSizeGiB(imageStat.Size())
	volumeBytes := sizeGiB * bytesPerGiB

	// Name the volume in the failure path: a part-written import leaves blocks
	// in predastore, and without the ID an operator cannot find them to reclaim.
	fmt.Println("Writing weights to storage ...")
	if err := admin.ImportImage(ctx, provider, admin.ImportOpts{
		VolumeID:         volumeId,
		NodeID:           nodeID,
		SizeBytes:        safecast.Uint64ToInt64(volumeBytes),
		AvailabilityZone: node.AZ,
		SourcePath:       imagePath,
		Snapshot:         true,
		Progress:         os.Stdout,
	}); err != nil {
		return "", fmt.Errorf("could not import weights volume %s: %w", volumeId, err)
	}

	// The provider wrote viperblock's half of the snapshot prefix; this writes
	// the EC2 control plane's, without which a launch cannot resolve it.
	snapshotID := admin.SnapPrefix(volumeId)
	store := objectstore.NewS3ObjectStoreFromConfig(node.Predastore.Host, node.Predastore.Region, node.Predastore.AccessKey, node.Predastore.SecretKey)
	if err := registerWeightsSnapshot(store, node.Predastore.Bucket, snapshotID, volumeId, volumeBytes, node.AZ, node.Viperblock.EncryptionKeyFile != ""); err != nil {
		return "", err
	}
	return snapshotID, nil
}

// newWeightsSnapshotChecker is the real weightsSnapshotChecker: it reads the
// EC2 control plane's snapshot document for snapshotID under the global
// account -- the same record registerWeightsSnapshot writes at stage time --
// and treats a missing object as gone. Any other read failure is a real
// error, not a signal to guess the snapshot's existence.
func newWeightsSnapshotChecker(store objectstore.ObjectStore, bucket string) weightsSnapshotChecker {
	metadata := ebsmetadata.NewStore(store, bucket)
	return func(ctx context.Context, snapshotID string) (bool, error) {
		if _, err := metadata.GetSnapshot(ctx, utils.GlobalAccountID, snapshotID); err != nil {
			if objectstore.IsNoSuchKeyError(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
}

// loadConfigAndConnectFn and ochreExit indirect the two effects that make
// the ochre weights Run functions otherwise untestable: a live NATS
// connection and a process-terminating exit. Tests substitute both -- a fake
// connection backed by an embedded JetStream server, and an exit stand-in
// that panics with a sentinel instead of killing the test binary -- so the
// same connect/validate/exit control flow real operators see is exercised
// directly.
var (
	loadConfigAndConnectFn = loadConfigAndConnect
	ochreExit              = os.Exit
)

func runOchreWeightsStage(cmd *cobra.Command, _ []string) {
	modelID, _ := cmd.Flags().GetString("model-id")
	s3URI, _ := cmd.Flags().GetString("s3-uri")
	tmpDirFlag, _ := cmd.Flags().GetString("tmp-dir")

	appConfig, nc, err := loadConfigAndConnectFn()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	defer nc.Close()
	node := appConfig.Nodes[appConfig.Node]

	js, err := jetstream.New(nc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	weightsStore := gateway_bedrock.NewWeightsStore(js, len(appConfig.Nodes))
	store := objectstore.NewS3ObjectStoreFromConfig(node.Predastore.Host, node.Predastore.Region, node.Predastore.AccessKey, node.Predastore.SecretKey)

	provider := ebsprovider.NewNATSProvider(nc, imageImportTimeout)
	materialize := func(ctx context.Context, downloadDir string, contentBytes int64) (string, error) {
		return materializeWeightsVolume(ctx, provider, appConfig.Node, node, downloadDir, contentBytes)
	}
	checkSnapshotLive := newWeightsSnapshotChecker(store, node.Predastore.Bucket)

	msg, err := runStageWeights(context.Background(), store, weightsStore, tmpDirFlag, modelID, s3URI, materialize, checkSnapshotLive)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		ochreExit(1)
		return
	}
	fmt.Println(msg)
}

// weightsStatusLive and weightsStatusDangling are the STATUS column values
// listWeightsOutput renders, so a dangling row (KV record present, snapshot
// gone) reads distinctly from a healthy one at a glance.
const (
	weightsStatusLive     = "OK"
	weightsStatusDangling = "DANGLING (snapshot missing)"
)

// listWeightsOutput renders 'ochre weights list': a friendly message when
// nothing is staged, else a MODEL ID / SOURCE URI / SNAPSHOT ID / STATUS
// table. checkSnapshotLive marks a row dangling when its KV record survives
// but the snapshot it names does not, so an operator sees at a glance which
// staged models can actually serve. Split out from runOchreWeightsList so it
// is testable against a real WeightsStore without a NATS connection.
func listWeightsOutput(ctx context.Context, weightsStore *gateway_bedrock.WeightsStore, checkSnapshotLive weightsSnapshotChecker) (string, error) {
	entries, err := weightsStore.ListWeights(ctx)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "No models staged.", nil
	}

	tableData := pterm.TableData{{"MODEL ID", "SOURCE URI", "REVISION", "SNAPSHOT ID", "STATUS"}}
	for _, e := range entries {
		live, err := checkSnapshotLive(ctx, e.SnapshotID)
		if err != nil {
			return "", err
		}
		status := weightsStatusLive
		if !live {
			status = weightsStatusDangling
		}
		revision := e.SourceRevision
		if revision == "" {
			revision = "-"
		}
		tableData = append(tableData, []string{e.ModelID, e.SourceURI, revision, e.SnapshotID, status})
	}
	return pterm.DefaultTable.WithHasHeader().WithLeftAlignment().WithData(tableData).Srender()
}

func runOchreWeightsList(_ *cobra.Command, _ []string) {
	appConfig, nc, err := loadConfigAndConnectFn()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	defer nc.Close()
	node := appConfig.Nodes[appConfig.Node]

	js, err := jetstream.New(nc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	weightsStore := gateway_bedrock.NewWeightsStore(js, len(appConfig.Nodes))
	store := objectstore.NewS3ObjectStoreFromConfig(node.Predastore.Host, node.Predastore.Region, node.Predastore.AccessKey, node.Predastore.SecretKey)
	checkSnapshotLive := newWeightsSnapshotChecker(store, node.Predastore.Bucket)

	out, err := listWeightsOutput(context.Background(), weightsStore, checkSnapshotLive)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	fmt.Println(out)
}

// removeWeights drops modelID's KV entry after confirming it exists, so the
// CLI can report a clear "nothing staged" error rather than a generic KV
// miss. It never touches the backing snapshot or source S3 objects -- only
// weightsStore.DeleteWeights's KV key is affected. Split out from
// runOchreWeightsRemove so it is testable against a real WeightsStore
// without a NATS connection.
func removeWeights(ctx context.Context, weightsStore *gateway_bedrock.WeightsStore, modelID string) (gateway_bedrock.WeightsEntry, error) {
	entry, ok, err := weightsStore.GetWeights(ctx, modelID)
	if err != nil {
		return gateway_bedrock.WeightsEntry{}, err
	}
	if !ok {
		return gateway_bedrock.WeightsEntry{}, fmt.Errorf("%s has no staged weights entry", modelID)
	}

	if err := weightsStore.DeleteWeights(ctx, modelID); err != nil {
		return gateway_bedrock.WeightsEntry{}, err
	}
	return entry, nil
}

func runOchreWeightsRemove(cmd *cobra.Command, _ []string) {
	modelID, _ := cmd.Flags().GetString("model-id")

	appConfig, nc, err := loadConfigAndConnectFn()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	weightsStore := gateway_bedrock.NewWeightsStore(js, len(appConfig.Nodes))

	entry, err := removeWeights(context.Background(), weightsStore, modelID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		ochreExit(1)
		return
	}
	fmt.Printf("✅ Removed staged-weights entry for %s (snapshot %s and source objects untouched).\n", modelID, entry.SnapshotID)
}

// resolveHFToken applies pull's token resolution order (D2): the --hf-token
// flag, then HF_TOKEN env (the must-have path, self-sufficient on its own),
// then a stored platform credential for vendor "huggingface". The stored
// credential step is a best-effort fallback: a cluster with no IAM master
// key provisioned yet simply yields no token rather than failing the pull.
func resolveHFToken(ctx context.Context, cmd *cobra.Command, cfg *config.ClusterConfig, nc *nats.Conn) string {
	if token, _ := cmd.Flags().GetString("hf-token"); token != "" {
		return token
	}
	if token := os.Getenv("HF_TOKEN"); token != "" {
		return token
	}

	masterKeyPath := filepath.Join(cfg.NodeBaseDir(), "config", "master.key")
	masterKey, err := masterkey.ReadShared(masterKeyPath)
	if err != nil {
		return ""
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return ""
	}
	credStore := gateway_bedrock.NewCredentialStore(js, masterKey, len(cfg.Nodes), nil)
	token, ok, err := credStore.Resolve(ctx, utils.GlobalAccountID, vendorHuggingFace)
	if err != nil || !ok {
		return ""
	}
	return token
}

func runOchreWeightsPull(cmd *cobra.Command, _ []string) {
	modelID, _ := cmd.Flags().GetString("model-id")
	hfRepo, _ := cmd.Flags().GetString("hf-repo")
	revision, _ := cmd.Flags().GetString("revision")
	revisionExplicit := cmd.Flags().Changed("revision")
	s3URI, _ := cmd.Flags().GetString("s3-uri")

	appConfig, nc, err := loadConfigAndConnectFn()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	defer nc.Close()
	node := appConfig.Nodes[appConfig.Node]

	ctx := context.Background()
	token := resolveHFToken(ctx, cmd, appConfig, nc)

	hf := hfhub.NewClient(token)
	store := objectstore.NewS3ObjectStoreFromConfig(node.Predastore.Host, node.Predastore.Region, node.Predastore.AccessKey, node.Predastore.SecretKey)

	finalURI, err := runPullWeights(ctx, hf, store, modelID, hfRepo, revision, revisionExplicit, s3URI)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		ochreExit(1)
		return
	}
	fmt.Printf("✅ Pulled into %s\n", finalURI)
	fmt.Println(finalURI)
}

// readTokenFromStdin reads a credential from r, trimming exactly one
// trailing newline (matching 'printf "%s" "$TOKEN" | cmd --token -' or a
// piped 'echo'). The token is never logged.
func readTokenFromStdin(r io.Reader) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("read token from stdin: %w", err)
	}
	token := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if token == "" {
		return "", fmt.Errorf("no token read from stdin")
	}
	return token, nil
}

// ochreCredentialsStore connects to the cluster, loads the node's IAM master
// key from disk -- the same on-disk key initIAMServiceFromConfig uses -- and
// returns a CredentialStore ready for PutCredential/Resolve.
func ochreCredentialsStore() (*gateway_bedrock.CredentialStore, func(), error) {
	cfg, nc, err := loadConfigAndConnectFn()
	if err != nil {
		return nil, nil, fmt.Errorf("connect to cluster: %w", err)
	}
	masterKeyPath := filepath.Join(cfg.NodeBaseDir(), "config", "master.key")
	masterKey, err := masterkey.ReadShared(masterKeyPath)
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("load master key: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("jetstream context: %w", err)
	}
	return gateway_bedrock.NewCredentialStore(js, masterKey, len(cfg.Nodes), nil), func() { nc.Close() }, nil
}

func runOchreCredentialsSet(cmd *cobra.Command, _ []string) {
	vendor, _ := cmd.Flags().GetString("vendor")
	accountID, _ := cmd.Flags().GetString("account")
	tokenFlag, _ := cmd.Flags().GetString("token")

	if tokenFlag != "-" {
		fmt.Fprintln(os.Stderr, "Error: --token must be '-' (the secret is read from stdin, never a command-line argument)")
		ochreExit(1)
		return
	}
	if accountID == "" {
		accountID = utils.GlobalAccountID
	}

	token, err := readTokenFromStdin(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}

	store, cleanup, err := ochreCredentialsStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	defer cleanup()

	if err := store.PutCredential(context.Background(), accountID, vendor, token); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	fmt.Printf("✅ Stored %s credential for account %s\n", vendor, accountID)
}

var adminOchreAccessCmd = &cobra.Command{
	Use:   "access",
	Short: "Manage per-account model access grants",
	Long: `Manage which accounts may see and invoke which Ochre models.

Access is deny-by-default: an account with no grants sees an empty model
catalog and every invocation is refused. Grants are cluster state held in the
bedrock-model-access KV bucket, so these commands need a running cluster but
may be run from any node.`,
}

var adminOchreAccessGrantCmd = &cobra.Command{
	Use:   "grant",
	Short: "Grant an account access to a model",
	Run:   runOchreAccessGrant,
}

var adminOchreAccessRevokeCmd = &cobra.Command{
	Use:   "revoke",
	Short: "Revoke an account's access to a model",
	Run:   runOchreAccessRevoke,
}

var adminOchreAccessListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the models an account has been granted",
	Run:   runOchreAccessList,
}

var adminOchreModelsCmd = &cobra.Command{
	Use:   "models",
	Short: "List every model in the platform catalog",
	Long: `List the full platform catalog, ignoring grants. This is what an operator can
grant from; it is not what any account can see.`,
	Run: runOchreModels,
}

func init() {
	ochreCmd.AddCommand(adminOchreAccessCmd)
	ochreCmd.AddCommand(adminOchreModelsCmd)
	adminOchreAccessCmd.AddCommand(adminOchreAccessGrantCmd)
	adminOchreAccessCmd.AddCommand(adminOchreAccessRevokeCmd)
	adminOchreAccessCmd.AddCommand(adminOchreAccessListCmd)

	for _, c := range []*cobra.Command{adminOchreAccessGrantCmd, adminOchreAccessRevokeCmd} {
		c.Flags().String("account-id", "", "12-digit account ID to change (required)")
		c.Flags().String("model-id", "", "Model ID to change (e.g. meta.llama3-2-1b-instruct-v1:0)")
		c.Flags().Bool("all-models", false, "Apply to every model in the platform catalog")
		_ = c.MarkFlagRequired("account-id")
	}

	adminOchreAccessListCmd.Flags().String("account-id", "", "12-digit account ID to inspect (required)")
	_ = adminOchreAccessListCmd.MarkFlagRequired("account-id")
}

// ochreAccessStore connects to the cluster and returns the grant store along
// with a cleanup that closes the connection.
func ochreAccessStore() (*gateway_bedrock.ModelAccessStore, func(), error) {
	cfg, nc, err := loadConfigAndConnectFn()
	if err != nil {
		return nil, nil, fmt.Errorf("connect to cluster: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("jetstream context: %w", err)
	}
	return gateway_bedrock.NewModelAccessStore(js, len(cfg.Nodes)), func() { nc.Close() }, nil
}

// ochreTargetModels resolves the --model-id / --all-models pair into the model
// set to act on. Exactly one of the two must be given: defaulting either way
// would make a mistyped flag silently change more or less than intended.
func ochreTargetModels(cmd *cobra.Command) ([]string, error) {
	modelID, _ := cmd.Flags().GetString("model-id")
	allModels, _ := cmd.Flags().GetBool("all-models")

	switch {
	case allModels && modelID != "":
		return nil, fmt.Errorf("--model-id and --all-models are mutually exclusive")
	case allModels:
		return gateway_bedrock.CatalogModelIDs(), nil
	case modelID != "":
		return []string{modelID}, nil
	default:
		return nil, fmt.Errorf("one of --model-id or --all-models is required")
	}
}

func runOchreAccessGrant(cmd *cobra.Command, _ []string) {
	runOchreAccessChange(cmd, true)
}

func runOchreAccessRevoke(cmd *cobra.Command, _ []string) {
	runOchreAccessChange(cmd, false)
}

// runOchreAccessChange applies a grant or revoke across the resolved model set.
// Both operations are idempotent, so re-running after a partial failure is safe
// and this is callable from provisioning that runs on every deploy.
func runOchreAccessChange(cmd *cobra.Command, grant bool) {
	accountID, _ := cmd.Flags().GetString("account-id")

	models, err := ochreTargetModels(cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}

	store, cleanup, err := ochreAccessStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	defer cleanup()

	ctx := context.Background()
	verb := "Granted"
	for _, modelID := range models {
		if grant {
			err = store.Grant(ctx, accountID, modelID)
		} else {
			verb = "Revoked"
			err = store.Revoke(ctx, accountID, modelID)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			ochreExit(1)
			return
		}
		fmt.Printf("✅ %s %s → %s\n", verb, accountID, modelID)
	}
}

func runOchreAccessList(cmd *cobra.Command, _ []string) {
	accountID, _ := cmd.Flags().GetString("account-id")

	store, cleanup, err := ochreAccessStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	defer cleanup()

	models, err := store.List(context.Background(), accountID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	if len(models) == 0 {
		fmt.Printf("Account %s has no model grants (it can see and invoke nothing).\n", accountID)
		return
	}

	// KV key order is not meaningful, so sort for a stable, diffable listing.
	sort.Strings(models)
	for _, modelID := range models {
		fmt.Println(modelID)
	}
}

func runOchreModels(_ *cobra.Command, _ []string) {
	models := gateway_bedrock.CatalogModelIDs()
	sort.Strings(models)
	for _, modelID := range models {
		fmt.Println(modelID)
	}
}
