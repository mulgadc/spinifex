//go:build e2e

package ecr

import (
	"archive/tar"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ociCmdTimeout = 2 * time.Minute

// writeLayerTar writes a single-file tar (an OCI layer) with random contents and
// returns its path.
func writeLayerTar(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "layer.tar")
	fh, err := os.Create(path) //nolint:gosec // test-controlled path under TmpDir
	require.NoError(t, err)
	defer fh.Close()

	payload := make([]byte, 8192)
	_, err = rand.Read(payload)
	require.NoError(t, err)

	tw := tar.NewWriter(fh)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "payload", Mode: 0o600, Size: int64(len(payload)),
	}))
	_, err = tw.Write(payload)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	return path
}

// craneLogin authenticates crane against host, writing the auth file under
// dockerConfig so subsequent crane calls reuse it.
func craneLogin(t *testing.T, crane *harness.ExternalCLI, host, pass string) {
	t.Helper()
	out, err := crane.Run(ociCmdTimeout, "auth", "login", host, "-u", "AWS", "-p", pass)
	require.NoErrorf(t, err, "crane auth login: %s", out)
}

// TestCranePushPull pushes a scratch image with `crane append` and pulls it
// back, exercising the Bearer /v2/token flow crane follows.
func TestCranePushPull(t *testing.T) {
	f := requireECRFixture(t)
	host := harness.ECRRegistryHost(f.Account)
	harness.RequireRegistryResolves(t, host)

	dockerConfig := filepath.Join(f.TmpDir, "crane")
	require.NoError(t, os.MkdirAll(dockerConfig, 0o700))
	crane := harness.NewCrane(t, dockerConfig)
	craneLogin(t, crane, host, harness.ECRGetLoginPassword(t, f.AWS))

	repo := uniqueRepo("crane")
	harness.CreateECRRepository(t, f.AWS, repo)
	ref := harness.ECRRepositoryURI(f.Account, repo) + ":v1"

	layer := writeLayerTar(t, f.TmpDir)
	out, err := crane.Run(ociCmdTimeout, "append", "-f", layer, "-t", ref)
	require.NoErrorf(t, err, "crane append: %s", out)

	require.True(t, harness.ECRWaitImageTag(t, f.AWS, repo, "v1", 30*time.Second),
		"crane-pushed tag v1 not visible via DescribeImages")

	digest, err := crane.Run(ociCmdTimeout, "digest", ref)
	require.NoErrorf(t, err, "crane digest: %s", digest)
	assert.Contains(t, digest, "sha256:")

	out, err = crane.Run(ociCmdTimeout, "pull", ref, filepath.Join(f.TmpDir, "pulled.tar"))
	require.NoErrorf(t, err, "crane pull: %s", out)
}

// TestSkopeoCopy seeds an image with crane, then `skopeo copy` retags it within
// the registry — exercising skopeo's Bearer auth on both read and write.
func TestSkopeoCopy(t *testing.T) {
	f := requireECRFixture(t)
	host := harness.ECRRegistryHost(f.Account)
	harness.RequireRegistryResolves(t, host)

	dockerConfig := filepath.Join(f.TmpDir, "skopeo")
	require.NoError(t, os.MkdirAll(dockerConfig, 0o700))
	crane := harness.NewCrane(t, dockerConfig)
	skopeo := harness.NewSkopeo(t, dockerConfig)

	pass := harness.ECRGetLoginPassword(t, f.AWS)
	craneLogin(t, crane, host, pass)

	repo := uniqueRepo("skopeo")
	harness.CreateECRRepository(t, f.AWS, repo)
	srcRef := harness.ECRRepositoryURI(f.Account, repo) + ":src"
	dstRef := harness.ECRRepositoryURI(f.Account, repo) + ":dst"

	layer := writeLayerTar(t, f.TmpDir)
	out, err := crane.Run(ociCmdTimeout, "append", "-f", layer, "-t", srcRef)
	require.NoErrorf(t, err, "crane append: %s", out)

	creds := "AWS:" + pass
	out, err = skopeo.Run(ociCmdTimeout, "copy",
		"--src-creds", creds, "--dest-creds", creds,
		"docker://"+srcRef, "docker://"+dstRef)
	require.NoErrorf(t, err, "skopeo copy: %s", out)

	require.True(t, harness.ECRWaitImageTag(t, f.AWS, repo, "dst", 30*time.Second),
		"skopeo-copied tag dst not visible via DescribeImages")
}
