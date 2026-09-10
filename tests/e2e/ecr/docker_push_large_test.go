//go:build e2e

package ecr

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// largeCmdTimeout accommodates the multi-hundred-MB build/push/pull cycle;
// dockerCmdTimeout (2m) is sized for the trivial single-layer fixture.
const largeCmdTimeout = 8 * time.Minute

// largeLayerSizesMiB targets getBlob's Content-Length forwarding for a real
// multi-hundred-MB object (gateway/ecr/registry.go), not a client chunk
// boundary — docker/crane always PATCH/PUT a whole blob in one request.
var largeLayerSizesMiB = []int{100, 80, 60}

// buildLargeScratchContext writes a hermetic FROM-scratch build context with
// one random-content layer per entry in sizesMiB, each its own COPY so it
// lands as a distinct blob (random content keeps the pushed size honest).
func buildLargeScratchContext(t *testing.T, dir string, sizesMiB []int) string {
	t.Helper()
	var dockerfile strings.Builder
	dockerfile.WriteString("FROM scratch\n")
	for i, mb := range sizesMiB {
		name := fmt.Sprintf("layer%d.bin", i)
		payload := make([]byte, mb<<20)
		_, err := rand.Read(payload)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), payload, 0o600))
		dockerfile.WriteString(fmt.Sprintf("COPY %s /%s\n", name, name))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile.String()), 0o600))
	return dir
}

// dockerImageDiffIDs returns ref's uncompressed layer digests (RootFS.Layers)
// via docker inspect, used to compare the built image against the re-pulled
// one at the layer level.
func dockerImageDiffIDs(t *testing.T, docker *harness.ExternalCLI, ref string) []string {
	t.Helper()
	out, err := docker.Run(largeCmdTimeout, "inspect", "--format", "{{json .RootFS.Layers}}", ref)
	require.NoErrorf(t, err, "docker inspect: %s", out)
	start, end := strings.Index(out, "["), strings.LastIndex(out, "]")
	require.True(t, start >= 0 && end > start, "unexpected docker inspect output: %s", out)
	var diffIDs []string
	require.NoError(t, json.Unmarshal([]byte(out[start:end+1]), &diffIDs))
	require.NotEmpty(t, diffIDs)
	return diffIDs
}

// ociManifestLayer / ociManifestDoc decode a manifest or index enough to find
// layer blobs, following an index's first entry to the image manifest it
// wraps (buildx can attach provenance/SBOM attestations as extra entries).
type ociManifestLayer struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

type ociManifestDoc struct {
	MediaType string             `json:"mediaType"`
	Layers    []ociManifestLayer `json:"layers"`
	Manifests []struct {
		Digest string `json:"digest"`
	} `json:"manifests"`
}

// fetchOCIManifest GETs name's manifest at reference over raw HTTP,
// authenticating with the ECR JWT directly as a Bearer token — the auth
// bridge accepts "Bearer <jwt>" without a /v2/token exchange.
func fetchOCIManifest(t *testing.T, aws *harness.AWSClient, host, name, reference, pass string) ociManifestDoc {
	t.Helper()
	status, _, body := harness.OCIRequest(t, aws, http.MethodGet, host,
		"/v2/"+name+"/manifests/"+reference, pass, nil)
	require.Equal(t, http.StatusOK, status, "GET manifest %s: %s", reference, body)
	var doc ociManifestDoc
	require.NoErrorf(t, json.Unmarshal(body, &doc), "decode manifest: %s", body)
	return doc
}

// resolveImageManifest follows an index down to the first image manifest it
// wraps, so the largest-layer search below sees real layers rather than an
// empty index document.
func resolveImageManifest(t *testing.T, aws *harness.AWSClient, host, name, reference, pass string) ociManifestDoc {
	t.Helper()
	doc := fetchOCIManifest(t, aws, host, name, reference, pass)
	for len(doc.Layers) == 0 && len(doc.Manifests) > 0 {
		doc = fetchOCIManifest(t, aws, host, name, doc.Manifests[0].Digest, pass)
	}
	require.NotEmpty(t, doc.Layers, "resolved manifest has no layers")
	return doc
}

// largestLayer returns the layer with the largest declared size.
func largestLayer(doc ociManifestDoc) ociManifestLayer {
	largest := doc.Layers[0]
	for _, l := range doc.Layers[1:] {
		if l.Size > largest.Size {
			largest = l
		}
	}
	return largest
}

// TestDockerPushPullLargeMultiLayer pushes a multi-hundred-MB, multi-layer
// image and re-pulls it to a genuinely cleared daemon, exercising storage
// paths the existing 4KB TestDockerPushPull cannot reach.
func TestDockerPushPullLargeMultiLayer(t *testing.T) {
	f := requireECRFixture(t)
	host := harness.ECRRegistryHost(f.Account)
	harness.RequireRegistryResolves(t, host)

	dockerConfig := filepath.Join(f.TmpDir, "docker-large")
	require.NoError(t, os.MkdirAll(dockerConfig, 0o700))
	docker := harness.NewDocker(t, dockerConfig)

	repo := uniqueRepo("docker-large")
	harness.CreateECRRepository(t, f.AWS, repo)
	ref := harness.ECRRepositoryURI(f.Account, repo) + ":v1"
	pass := harness.ECRGetLoginPassword(t, f.AWS)

	t.Run("docker login", func(t *testing.T) {
		out, err := docker.RunStdin(dockerCmdTimeout, pass,
			"login", host, "-u", "AWS", "--password-stdin")
		require.NoErrorf(t, err, "docker login: %s", out)
		assert.Contains(t, out, "Login Succeeded")
	})

	var pushedDiffIDs []string

	t.Run("build multi-layer image + push", func(t *testing.T) {
		buildDir := filepath.Join(f.TmpDir, "build-large")
		require.NoError(t, os.MkdirAll(buildDir, 0o700))
		ctxDir := buildLargeScratchContext(t, buildDir, largeLayerSizesMiB)

		out, err := docker.Run(largeCmdTimeout, "build", "-t", ref, ctxDir)
		require.NoErrorf(t, err, "docker build: %s", out)

		pushedDiffIDs = dockerImageDiffIDs(t, docker, ref)
		require.Len(t, pushedDiffIDs, len(largeLayerSizesMiB))

		out, err = docker.Run(largeCmdTimeout, "push", ref)
		require.NoErrorf(t, err, "docker push: %s", out)
	})

	t.Run("DescribeImages shows the pushed tag", func(t *testing.T) {
		require.True(t, harness.ECRWaitImageTag(t, f.AWS, repo, "v1", 60*time.Second),
			"pushed tag v1 not visible via DescribeImages")
	})

	var largest ociManifestLayer

	t.Run("raw HTTP GET on the largest blob returns a non-zero Content-Length", func(t *testing.T) {
		doc := resolveImageManifest(t, f.AWS, host, repo, "v1", pass)
		largest = largestLayer(doc)
		require.Greater(t, largest.Size, int64(50<<20), "largest layer smaller than expected")

		// Direct regression check for a reported defect where a large blob
		// GET returned 200 with content-length: 0 — a recurrence fails
		// both assertions below.
		status, headers, body := harness.OCIRequest(t, f.AWS, http.MethodGet, host,
			"/v2/"+repo+"/blobs/"+largest.Digest, pass, nil)
		require.Equal(t, http.StatusOK, status, "GET blob %s", largest.Digest)

		cl := headers.Get("Content-Length")
		require.NotEmpty(t, cl, "no Content-Length header on large blob GET")
		assert.NotEqual(t, "0", cl, "content-length 0 on large blob GET (regression)")
		assert.Equal(t, largest.Size, int64(len(body)),
			"GET body length does not match the manifest-declared blob size")
	})

	t.Run("docker pull round-trips from a genuinely cleared daemon", func(t *testing.T) {
		out, err := docker.Run(largeCmdTimeout, "rmi", "-f", ref)
		require.NoErrorf(t, err, "docker rmi: %s", out)

		// docker rmi alone is not a valid cache-bust: buildx's build cache
		// keeps the layer content addressable and can silently satisfy the
		// next pull without touching the registry, producing a false pass.
		out, err = docker.Run(largeCmdTimeout, "builder", "prune", "-af")
		require.NoErrorf(t, err, "docker builder prune: %s", out)

		out, err = docker.Run(largeCmdTimeout, "pull", ref)
		require.NoErrorf(t, err, "docker pull: %s", out)

		pulledDiffIDs := dockerImageDiffIDs(t, docker, ref)
		assert.Equal(t, pushedDiffIDs, pulledDiffIDs,
			"re-pulled image layer digests do not match the pushed image")
	})
}
