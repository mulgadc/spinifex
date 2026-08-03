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
// *.safetensors file (variable name) is checked separately.
var requiredWeightsFiles = []string{
	"config.json",
	"tokenizer_config.json",
	"tokenizer.json",
	"tokenizer.model",
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

// buildWeightsImage packages srcDir's files into a raw ext4 filesystem image
// at imagePath, sized to fit their total bytes plus filesystem overhead and
// headroom. mkfs.ext4 -d populates the filesystem directly from srcDir, so
// no loopback mount (and no root) is needed -- unlike the guestfish/
// virt-customize tooling build-system-image.sh needs to customize a
// bootable cloud image, a weights volume is just a directory of files.
func buildWeightsImage(srcDir, imagePath string, contentBytes int64) error {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		return fmt.Errorf("mkfs.ext4 not found: %w", err)
	}

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

	out, err := exec.Command("mkfs.ext4", "-F", "-d", srcDir, imagePath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4: %w: %s", err, string(out))
	}
	return nil
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

func runOchreWeightsStage(cmd *cobra.Command, _ []string) {
	modelID, _ := cmd.Flags().GetString("model-id")
	s3URI, _ := cmd.Flags().GetString("s3-uri")
	tmpDirFlag, _ := cmd.Flags().GetString("tmp-dir")

	if _, found, selfHost := gateway_bedrock.LookupServingSpec(modelID); !found || !selfHost {
		if !found {
			fmt.Fprintf(os.Stderr, "Unknown model ID %q: not present in the Ochre catalog\n", modelID)
		} else {
			fmt.Fprintf(os.Stderr, "%q is a provider-served model, not self-host; weights staging does not apply\n", modelID)
		}
		os.Exit(1)
	}

	bucket, prefix, err := parseWeightsS3URI(s3URI)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	sourceURI := fmt.Sprintf("s3://%s/%s", bucket, prefix)

	appConfig, nc, err := loadConfigAndConnect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer nc.Close()
	node := appConfig.Nodes[appConfig.Node]

	js, err := jetstream.New(nc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	weightsStore := gateway_bedrock.NewWeightsStore(js, len(appConfig.Nodes))

	ctx := context.Background()
	existing, hadPrevious, err := weightsStore.GetWeights(ctx, modelID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if hadPrevious && existing.SourceURI == sourceURI {
		fmt.Printf("%s is already staged from %s (snapshot %s); nothing to do.\n", modelID, sourceURI, existing.SnapshotID)
		return
	}

	store := objectstore.NewS3ObjectStoreFromConfig(node.Predastore.Host, node.Predastore.Region, node.Predastore.AccessKey, node.Predastore.SecretKey)

	fmt.Printf("Validating %s ...\n", sourceURI)
	if err := validateWeightsPrefix(ctx, store, bucket, prefix); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	tmpDir, err := os.MkdirTemp(tmpDirFlag, "spinifex-weights-tmp-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not create temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	downloadDir := filepath.Join(tmpDir, "src")
	if err := os.MkdirAll(downloadDir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "Could not create download dir: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Downloading %s ...\n", sourceURI)
	contentBytes, err := downloadWeightsPrefix(ctx, store, bucket, prefix, downloadDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	volumeId := utils.GenerateResourceID("vol")
	imagePath := filepath.Join(tmpDir, volumeId+".img")

	fmt.Println("Building filesystem image ...")
	if err := buildWeightsImage(downloadDir, imagePath, contentBytes); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	imageStat, err := os.Stat(imagePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not stat image: %v\n", err)
		os.Exit(1)
	}

	s3Config := vbs3.S3Config{
		VolumeName: volumeId,
		VolumeSize: utils.SafeInt64ToUint64(imageStat.Size()),
		Bucket:     node.Predastore.Bucket,
		Region:     node.Predastore.Region,
		AccessKey:  node.Predastore.AccessKey,
		SecretKey:  node.Predastore.SecretKey,
		Host:       node.Predastore.Host,
	}

	mkey, err := utils.LoadViperblockMasterKey(node.Viperblock.EncryptionKeyFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not load viperblock encryption key: %v\n", err)
		os.Exit(1)
	}

	// AMIMetadata is deliberately left zero-valued: a weights volume is not
	// bootable and must never be registered as a launchable AMI. Leaving it
	// unset also means ImportDiskImage does not attempt its own automatic
	// snapshot -- that happens explicitly below, after the volume is closed.
	manifest := viperblock.VolumeConfig{}
	manifest.VolumeMetadata.VolumeID = volumeId
	manifest.VolumeMetadata.VolumeName = volumeId
	manifest.VolumeMetadata.TenantID = "system"
	manifest.VolumeMetadata.SizeGiB = amiVolumeSizeGiB(imageStat.Size())
	manifest.VolumeMetadata.State = "available"
	manifest.VolumeMetadata.AvailabilityZone = node.Predastore.Region
	manifest.VolumeMetadata.CreatedAt = time.Now()
	manifest.VolumeMetadata.VolumeType = "gp3"
	manifest.VolumeMetadata.IOPS = 1000

	vbConfig := viperblock.VB{
		VolumeName:        volumeId,
		VolumeSize:        utils.SafeInt64ToUint64(imageStat.Size()),
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

	if err := v_utils.ImportDiskImage(&s3Config, &vbConfig, imagePath, progress); err != nil {
		if flushBar != nil {
			_, _ = flushBar.Stop()
		}
		fmt.Fprintf(os.Stderr, "Could not import weights volume: %v\n", err)
		os.Exit(1)
	}
	if flushBar != nil {
		_, _ = flushBar.Stop()
	}

	fmt.Println("Snapshotting weights volume ...")
	snapshotID, err := snapshotImportedWeightsVolume(&s3Config, volumeId, utils.SafeInt64ToUint64(imageStat.Size()), tmpDir, mkey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not snapshot weights volume: %v\n", err)
		os.Exit(1)
	}

	if err := weightsStore.PutWeights(ctx, modelID, sourceURI, snapshotID); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if hadPrevious {
		fmt.Printf("✅ Staged %s from %s (snapshot %s). Replaced previous snapshot %s -- reclaim it separately if no longer needed.\n",
			modelID, sourceURI, snapshotID, existing.SnapshotID)
	} else {
		fmt.Printf("✅ Staged %s from %s (snapshot %s).\n", modelID, sourceURI, snapshotID)
	}
}

func runOchreWeightsList(_ *cobra.Command, _ []string) {
	appConfig, nc, err := loadConfigAndConnect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	weightsStore := gateway_bedrock.NewWeightsStore(js, len(appConfig.Nodes))

	entries, err := weightsStore.ListWeights(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if len(entries) == 0 {
		fmt.Println("No models staged.")
		return
	}

	tableData := pterm.TableData{{"MODEL ID", "SOURCE URI", "SNAPSHOT ID"}}
	for _, e := range entries {
		tableData = append(tableData, []string{e.ModelID, e.SourceURI, e.SnapshotID})
	}
	pterm.DefaultTable.WithHasHeader().WithLeftAlignment().WithData(tableData).Render()
}

func runOchreWeightsRemove(cmd *cobra.Command, _ []string) {
	modelID, _ := cmd.Flags().GetString("model-id")

	appConfig, nc, err := loadConfigAndConnect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	weightsStore := gateway_bedrock.NewWeightsStore(js, len(appConfig.Nodes))

	ctx := context.Background()
	entry, ok, err := weightsStore.GetWeights(ctx, modelID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "%s has no staged weights entry.\n", modelID)
		os.Exit(1)
	}

	if err := weightsStore.DeleteWeights(ctx, modelID); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✅ Removed staged-weights entry for %s (snapshot %s and source objects untouched).\n", modelID, entry.SnapshotID)
}
