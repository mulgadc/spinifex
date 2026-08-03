package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	putObject(t, store, "models", "llama-3.2-1b/tokenizer_config.json", nil)
	// tokenizer.json and tokenizer.model deliberately omitted.

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
