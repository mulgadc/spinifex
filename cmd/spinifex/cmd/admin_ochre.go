package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/predastore/pkg/masterkey"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/config"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/viperblock/viperblock"
	vbs3 "github.com/mulgadc/viperblock/viperblock/backends/s3"
	"github.com/mulgadc/viperblock/viperblock/v_utils"
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

func init() {
	adminCmd.AddCommand(ochreCmd)
	ochreCmd.AddCommand(ochreWeightsCmd)
	ochreWeightsCmd.AddCommand(ochreWeightsStageCmd)
	ochreWeightsCmd.AddCommand(ochreWeightsListCmd)
	ochreWeightsCmd.AddCommand(ochreWeightsRemoveCmd)

	ochreWeightsStageCmd.Flags().String("model-id", "", "Catalog model ID to stage weights for (required)")
	ochreWeightsStageCmd.Flags().String("s3-uri", "", "predastore S3 URI holding the model's Hugging Face files, e.g. s3://bucket/prefix/ (required)")
	ochreWeightsStageCmd.Flags().String("tmp-dir", os.TempDir(), "Temporary directory for download and volume staging")
	_ = ochreWeightsStageCmd.MarkFlagRequired("model-id")
	_ = ochreWeightsStageCmd.MarkFlagRequired("s3-uri")

	ochreWeightsRemoveCmd.Flags().String("model-id", "", "Model ID to remove from staged weights (required)")
	_ = ochreWeightsRemoveCmd.MarkFlagRequired("model-id")
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

// snapshotImportedWeightsVolume snapshots a viperblock volume that
// v_utils.ImportDiskImage just wrote and closed -- never attached, never
// nbdkit-served, so there is no live checkpoint to load. This mirrors the
// offline snapshot sequence handlers/ec2/image/service_impl.go's
// snapshotStoppedVolume uses for a stopped instance's root volume: reopen
// read-only, load the numbered checkpoint Close() wrote, then CreateSnapshot.
func snapshotImportedWeightsVolume(s3Config *vbs3.S3Config, volumeID string, volumeSize uint64, walDir string, mkey *masterkey.Key) (string, error) {
	vbConfig := viperblock.VB{
		VolumeName:        volumeID,
		VolumeSize:        volumeSize,
		BaseDir:           walDir,
		Cache:             viperblock.Cache{Config: viperblock.CacheConfig{Size: 0}},
		MasterKey:         mkey,
		EncryptionEnabled: mkey != nil,
		Logger:            slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	vb, err := viperblock.New(&vbConfig, "s3", *s3Config)
	if err != nil {
		return "", fmt.Errorf("new viperblock: %w", err)
	}
	defer func() {
		vb.StopChunkUploader()
		vb.StopWALSyncer()
	}()

	if err := vb.Backend.Init(); err != nil {
		return "", fmt.Errorf("backend init: %w", err)
	}
	if err := vb.LoadState(); err != nil {
		return "", fmt.Errorf("load state: %w", err)
	}
	if err := vb.LoadBlockState(); err != nil {
		return "", fmt.Errorf("load block state: %w", err)
	}
	defer func() {
		if err := vb.RemoveLocalFiles(); err != nil {
			slog.Warn("snapshotImportedWeightsVolume: failed to remove local files", "volumeId", volumeID, "err", err)
		}
	}()

	snapshotID := admin.SnapPrefix(volumeID)
	if _, err := vb.CreateSnapshot(snapshotID); err != nil {
		return "", fmt.Errorf("create snapshot: %w", err)
	}
	return snapshotID, nil
}

// weightsMaterializer builds a servable snapshot from downloadDir's already
// downloaded and validated Hugging Face files. Isolating this behind a
// function value lets runStageWeights be tested end to end -- including the
// idempotent-noop and replace-and-report-previous-snapshot decisions -- with
// a fake standing in for the real mkfs.ext4 + viperblock + predastore work,
// which needs a live environment (see materializeWeightsVolume).
type weightsMaterializer func(ctx context.Context, downloadDir string, contentBytes int64) (snapshotID string, err error)

// runStageWeights holds 'ochre weights stage' decision and side-effect logic:
// catalog validation, S3 URI parsing, idempotency/replacement detection
// against the existing KV record, prefix validation, download, materialisation
// via materialize, and the final KV write. It has no cobra or NATS-connection
// dependency of its own, so tests drive it directly against a fake object
// store, a real WeightsStore backed by an embedded JetStream server, and a
// fake materializer.
func runStageWeights(ctx context.Context, store objectstore.ObjectStore, weightsStore *gateway_bedrock.WeightsStore, tmpDirFlag, modelID, s3URI string, materialize weightsMaterializer) (string, error) {
	if _, found, selfHost := gateway_bedrock.LookupServingSpec(modelID); !found || !selfHost {
		if !found {
			return "", fmt.Errorf("unknown model ID %q: not present in the Ochre catalog", modelID)
		}
		return "", fmt.Errorf("%q is a provider-served model, not self-host; weights staging does not apply", modelID)
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
	if hadPrevious && existing.SourceURI == sourceURI {
		return fmt.Sprintf("%s is already staged from %s (snapshot %s); nothing to do.", modelID, sourceURI, existing.SnapshotID), nil
	}

	fmt.Printf("Validating %s ...\n", sourceURI)
	if err := validateWeightsPrefix(ctx, store, bucket, prefix); err != nil {
		return "", err
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

	if err := weightsStore.PutWeights(ctx, modelID, sourceURI, snapshotID); err != nil {
		return "", err
	}

	if hadPrevious {
		return fmt.Sprintf("✅ Staged %s from %s (snapshot %s). Replaced previous snapshot %s -- reclaim it separately if no longer needed.",
			modelID, sourceURI, snapshotID, existing.SnapshotID), nil
	}
	return fmt.Sprintf("✅ Staged %s from %s (snapshot %s).", modelID, sourceURI, snapshotID), nil
}

// materializeWeightsVolume is the real weightsMaterializer: it packages
// downloadDir into a filesystem image, imports it into a new viperblock
// volume backed by node's predastore, and snapshots it. This is the
// expensive, environment-dependent half of stage -- it shells out to
// mkfs.ext4 and talks to a live S3-shaped backend -- so it is exercised live
// (see the plan's Testing section) rather than in unit tests.
func materializeWeightsVolume(node config.Config, downloadDir string, contentBytes int64, mkey *masterkey.Key) (string, error) {
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

	// Round the volume up to a whole GiB rather than using the raw image size.
	// viperblock rejects a tail write whose block overruns VolumeSize, so an
	// unaligned size fails at the last block. This also keeps the byte size and
	// the manifest's SizeGiB describing the same volume.
	sizeGiB := amiVolumeSizeGiB(imageStat.Size())
	volumeBytes := sizeGiB * bytesPerGiB

	s3Config := vbs3.S3Config{
		VolumeName: volumeId,
		VolumeSize: volumeBytes,
		Bucket:     node.Predastore.Bucket,
		Region:     node.Predastore.Region,
		AccessKey:  node.Predastore.AccessKey,
		SecretKey:  node.Predastore.SecretKey,
		Host:       node.Predastore.Host,
	}

	// AMIMetadata is deliberately left zero-valued: a weights volume is not
	// bootable and must never be registered as a launchable AMI. Leaving it
	// unset also means ImportDiskImage does not attempt its own automatic
	// snapshot -- that happens explicitly below, after the volume is closed.
	manifest := viperblock.VolumeConfig{}
	manifest.VolumeMetadata.VolumeID = volumeId
	manifest.VolumeMetadata.VolumeName = volumeId
	manifest.VolumeMetadata.TenantID = "system"
	manifest.VolumeMetadata.SizeGiB = sizeGiB
	manifest.VolumeMetadata.State = "available"
	manifest.VolumeMetadata.AvailabilityZone = node.Predastore.Region
	manifest.VolumeMetadata.CreatedAt = time.Now()
	manifest.VolumeMetadata.VolumeType = "gp3"
	manifest.VolumeMetadata.IOPS = 1000

	vbConfig := viperblock.VB{
		VolumeName:        volumeId,
		VolumeSize:        volumeBytes,
		BaseDir:           tmpDir,
		Cache:             viperblock.Cache{Config: viperblock.CacheConfig{Size: 0}},
		VolumeConfig:      manifest,
		MasterKey:         mkey,
		EncryptionEnabled: mkey != nil,
		Logger:            slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	var flushBar *pterm.ProgressbarPrinter
	var flushUpdate func(current uint64)
	progress := func(current, total uint64) {
		if flushBar == nil {
			flushBar, flushUpdate = utils.NewByteProgressBar("Flushing weights to storage", total)
		}
		flushUpdate(current)
	}

	// Name the volume in the failure path: a part-written import leaves blocks
	// in predastore, and without the ID an operator cannot find them to reclaim.
	if err := v_utils.ImportDiskImage(&s3Config, &vbConfig, imagePath, progress); err != nil {
		if flushBar != nil {
			_, _ = flushBar.Stop()
		}
		return "", fmt.Errorf("could not import weights volume %s: %w", volumeId, err)
	}
	if flushBar != nil {
		_, _ = flushBar.Stop()
	}

	fmt.Println("Snapshotting weights volume ...")
	snapshotID, err := snapshotImportedWeightsVolume(&s3Config, volumeId, volumeBytes, tmpDir, mkey)
	if err != nil {
		return "", fmt.Errorf("could not snapshot weights volume: %w", err)
	}
	return snapshotID, nil
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

	mkey, err := utils.LoadViperblockMasterKey(node.Viperblock.EncryptionKeyFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not load viperblock encryption key: %v\n", err)
		ochreExit(1)
		return
	}

	materialize := func(_ context.Context, downloadDir string, contentBytes int64) (string, error) {
		return materializeWeightsVolume(node, downloadDir, contentBytes, mkey)
	}

	msg, err := runStageWeights(context.Background(), store, weightsStore, tmpDirFlag, modelID, s3URI, materialize)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		ochreExit(1)
		return
	}
	fmt.Println(msg)
}

// listWeightsOutput renders 'ochre weights list': a friendly message when
// nothing is staged, else a MODEL ID / SOURCE URI / SNAPSHOT ID table. Split
// out from runOchreWeightsList so it is testable against a real WeightsStore
// without a NATS connection.
func listWeightsOutput(ctx context.Context, weightsStore *gateway_bedrock.WeightsStore) (string, error) {
	entries, err := weightsStore.ListWeights(ctx)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "No models staged.", nil
	}

	tableData := pterm.TableData{{"MODEL ID", "SOURCE URI", "SNAPSHOT ID"}}
	for _, e := range entries {
		tableData = append(tableData, []string{e.ModelID, e.SourceURI, e.SnapshotID})
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

	js, err := jetstream.New(nc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	weightsStore := gateway_bedrock.NewWeightsStore(js, len(appConfig.Nodes))

	out, err := listWeightsOutput(context.Background(), weightsStore)
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
