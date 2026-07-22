//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file covers the ECR data plane — the OCI Distribution v2 surface the
// live ecr/ suite drove through docker, crane and skopeo. Those tests need no
// guest: they need a registry, an object store and an HTTP client, all of which
// the in-process gateway already provides on an ephemeral port. Running them
// here means a registry regression fails in the front gate rather than after a
// guest boot, and removes the last reason the ecr suite had to sit behind full
// QEMU provisioning.
//
// What is deliberately NOT moved down: the capacity/load leg, which exists to
// put real parallel pressure on a real predastore. A memory-backed object store
// cannot make that claim, so it stays live.

// pushBlob uploads one blob through the two-step Distribution upload flow and
// returns its digest. Split out from seedOCIRepo (which pushes a fixed
// single-blob fixture) because these tests push several blobs and then read the
// exact bytes back.
func pushBlob(t *testing.T, gw *Gateway, repo, bearer string, data []byte) string {
	t.Helper()
	digest := ociDigest(data)

	status, hdr, body := ecrOCIRequest(t, gw, http.MethodPost, "/v2/"+repo+"/blobs/uploads/", bearer, nil)
	require.Equal(t, http.StatusAccepted, status, "start blob upload: %s", body)
	loc := hdr.Get("Location")
	require.NotEmpty(t, loc, "upload response missing Location header")

	status, _, body = ecrOCIRequest(t, gw, http.MethodPut, loc+"?digest="+digest, bearer, data)
	require.Equal(t, http.StatusCreated, status, "finish blob upload: %s", body)
	return digest
}

// pushManifest publishes a manifest referencing cfgDigest and layerDigests at
// tag, returning the digest the registry assigned it.
func pushManifest(t *testing.T, gw *Gateway, repo, bearer, tag, cfgDigest string, layerDigests ...string) string {
	t.Helper()

	layers := ""
	for i, d := range layerDigests {
		if i > 0 {
			layers += ","
		}
		layers += fmt.Sprintf(`{"digest":"%s"}`, d)
	}
	manifest := fmt.Appendf(nil,
		`{"schemaVersion":2,"mediaType":"%s","config":{"digest":"%s"},"layers":[%s]}`,
		ociManifestMediaType, cfgDigest, layers)

	status, hdr, body := ecrOCIRequest(t, gw, http.MethodPut, "/v2/"+repo+"/manifests/"+tag, bearer, manifest)
	require.Equal(t, http.StatusCreated, status, "push manifest: %s", body)
	digest := hdr.Get("Docker-Content-Digest")
	require.NotEmpty(t, digest, "manifest push did not return Docker-Content-Digest")
	return digest
}

// TestECRRegistryPushPullRoundTrip is the integration-tier equivalent of the
// live docker/crane push-pull tests: push a config blob and a layer blob, push
// a manifest referencing both, then read all three back and assert the bytes
// are byte-identical to what was pushed.
//
// Asserting the returned bytes rather than only the status codes is the point.
// A registry that stores a blob under the wrong digest, truncates it, or serves
// a different blob still answers 200 to every request in this sequence; only
// comparing content catches it.
func TestECRRegistryPushPullRoundTrip(t *testing.T) {
	gw := StartGateway(t)
	StartECRDaemonLite(t, gw)

	repo := uniqueName("roundtrip")
	_, err := gw.ECRClient(t).CreateRepository(&ecr.CreateRepositoryInput{RepositoryName: aws.String(repo)})
	require.NoError(t, err, "create-repository")
	bearer := ecrGetLoginPassword(t, gw.ECRClient(t))

	cfg := []byte(`{"architecture":"amd64","os":"linux"}`)
	layer := []byte("integration-tier layer payload, deliberately not empty")
	cfgDigest := pushBlob(t, gw, repo, bearer, cfg)
	layerDigest := pushBlob(t, gw, repo, bearer, layer)
	manifestDigest := pushManifest(t, gw, repo, bearer, "v1", cfgDigest, layerDigest)

	// Pull each blob back and compare content, not just status.
	for name, want := range map[string][]byte{cfgDigest: cfg, layerDigest: layer} {
		status, _, got := ecrOCIRequest(t, gw, http.MethodGet, "/v2/"+repo+"/blobs/"+name, bearer, nil)
		require.Equal(t, http.StatusOK, status, "pull blob %s", name)
		assert.Equal(t, want, got, "blob %s read back with different content than was pushed", name)
	}

	// The manifest must be retrievable by tag and by digest, and both must
	// agree — a registry that resolves a tag to a stale manifest still serves
	// 200 for each request taken on its own.
	statusTag, _, byTag := ecrOCIRequest(t, gw, http.MethodGet, "/v2/"+repo+"/manifests/v1", bearer, nil)
	require.Equal(t, http.StatusOK, statusTag, "pull manifest by tag: %s", byTag)
	statusDigest, _, byDigest := ecrOCIRequest(t, gw, http.MethodGet, "/v2/"+repo+"/manifests/"+manifestDigest, bearer, nil)
	require.Equal(t, http.StatusOK, statusDigest, "pull manifest by digest: %s", byDigest)
	assert.Equal(t, byTag, byDigest, "tag v1 and digest %s resolve to different manifests", manifestDigest)
	assert.Equal(t, manifestDigest, ociDigest(byTag), "served manifest does not hash to the digest the registry assigned it")

	// The control plane must see what the data plane stored; the live suite
	// asserted this to catch a push that lands in the object store without a
	// metadata record.
	out, err := gw.ECRClient(t).DescribeImages(&ecr.DescribeImagesInput{RepositoryName: aws.String(repo)})
	require.NoError(t, err, "describe-images")
	var tags []string
	for _, d := range out.ImageDetails {
		tags = append(tags, aws.StringValueSlice(d.ImageTags)...)
	}
	assert.Contains(t, tags, "v1", "pushed tag not visible via DescribeImages")
}

// TestECRRegistryPushToUncreatedRepoRejected asserts the registry does not
// auto-create a repository on push. Auto-creation is the permissive default in
// some registries, and it would silently defeat repository-level authorization:
// a principal denied on every existing repo could simply push to a new name.
func TestECRRegistryPushToUncreatedRepoRejected(t *testing.T) {
	gw := StartGateway(t)
	StartECRDaemonLite(t, gw)

	// A valid token for the registry, but a repository that was never created.
	bearer := ecrGetLoginPassword(t, gw.ECRClient(t))
	repo := uniqueName("never-created")

	status, _, body := ecrOCIRequest(t, gw, http.MethodPost, "/v2/"+repo+"/blobs/uploads/", bearer, nil)
	assert.NotEqual(t, http.StatusAccepted, status,
		"registry accepted an upload for a repository that was never created (auto-create would bypass per-repository authorization): %s", body)

	// And it must not have been created as a side effect of the attempt.
	_, err := gw.ECRClient(t).DescribeRepositories(&ecr.DescribeRepositoriesInput{
		RepositoryNames: []*string{aws.String(repo)},
	})
	assert.Error(t, err, "repository %s exists after a rejected push", repo)
}

// TestECRRegistryRetagWithinRegistry covers what the live skopeo-copy test
// covered: re-publishing an existing manifest under a second tag. Both tags
// must resolve to the same digest, and the original must keep working — a
// retag that rewrites rather than aliases would break the first tag.
func TestECRRegistryRetagWithinRegistry(t *testing.T) {
	gw := StartGateway(t)
	StartECRDaemonLite(t, gw)

	repo := uniqueName("retag")
	_, err := gw.ECRClient(t).CreateRepository(&ecr.CreateRepositoryInput{RepositoryName: aws.String(repo)})
	require.NoError(t, err, "create-repository")
	bearer := ecrGetLoginPassword(t, gw.ECRClient(t))

	cfg := []byte(`{"architecture":"amd64","os":"linux","note":"retag"}`)
	cfgDigest := pushBlob(t, gw, repo, bearer, cfg)
	v1Digest := pushManifest(t, gw, repo, bearer, "v1", cfgDigest)

	// Re-publish the identical manifest bytes under a second tag, which is what
	// a copy within the same registry reduces to.
	_, _, manifestBytes := ecrOCIRequest(t, gw, http.MethodGet, "/v2/"+repo+"/manifests/v1", bearer, nil)
	status, hdr, body := ecrOCIRequest(t, gw, http.MethodPut, "/v2/"+repo+"/manifests/v2", bearer, manifestBytes)
	require.Equal(t, http.StatusCreated, status, "push retagged manifest: %s", body)
	assert.Equal(t, v1Digest, hdr.Get("Docker-Content-Digest"), "identical manifest content produced a different digest under the new tag")

	// The original tag must still resolve, and to the same manifest.
	statusV1, _, stillV1 := ecrOCIRequest(t, gw, http.MethodGet, "/v2/"+repo+"/manifests/v1", bearer, nil)
	require.Equal(t, http.StatusOK, statusV1, "original tag stopped resolving after retag: %s", stillV1)
	assert.Equal(t, manifestBytes, stillV1, "original tag resolves to different content after retag")
}
