//go:build e2e

package ecr

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dockerCmdTimeout = 2 * time.Minute

// buildScratchContext writes a hermetic FROM-scratch build context (no network
// pulls) with a random payload layer, returning the context dir.
func buildScratchContext(t *testing.T, dir string) string {
	t.Helper()
	payload := make([]byte, 4096)
	_, err := rand.Read(payload)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "payload"), payload, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"),
		[]byte("FROM scratch\nCOPY payload /payload\n"), 0o600))
	return dir
}

// TestDockerPushPull drives the canonical workflow: aws ecr get-login-password
// -> docker login -> docker push -> DescribeImages -> docker pull (clean daemon)
// against the cluster registry.
func TestDockerPushPull(t *testing.T) {
	f := requireECRFixture(t)
	host := harness.ECRRegistryHost(f.Account)
	harness.RequireRegistryResolves(t, host)

	dockerConfig := filepath.Join(f.TmpDir, "docker")
	require.NoError(t, os.MkdirAll(dockerConfig, 0o700))
	docker := harness.NewDocker(t, dockerConfig)

	repo := uniqueRepo("docker")
	harness.CreateECRRepository(t, f.AWS, repo)
	ref := harness.ECRRepositoryURI(f.Account, repo) + ":v1"

	t.Run("docker login", func(t *testing.T) {
		pass := harness.ECRGetLoginPassword(t, f.AWS)
		out, err := docker.RunStdin(dockerCmdTimeout, pass,
			"login", host, "-u", "AWS", "--password-stdin")
		require.NoErrorf(t, err, "docker login: %s", out)
		assert.Contains(t, out, "Login Succeeded")
	})

	t.Run("docker build + push", func(t *testing.T) {
		buildDir := filepath.Join(f.TmpDir, "build")
		require.NoError(t, os.MkdirAll(buildDir, 0o700))
		ctxDir := buildScratchContext(t, buildDir)

		out, err := docker.Run(dockerCmdTimeout, "build", "-t", ref, ctxDir)
		require.NoErrorf(t, err, "docker build: %s", out)

		out, err = docker.Run(dockerCmdTimeout, "push", ref)
		require.NoErrorf(t, err, "docker push: %s", out)
	})

	t.Run("DescribeImages shows the pushed tag", func(t *testing.T) {
		require.True(t, harness.ECRWaitImageTag(t, f.AWS, repo, "v1", 30*time.Second),
			"pushed tag v1 not visible via DescribeImages")
	})

	t.Run("docker pull round-trips from a clean daemon", func(t *testing.T) {
		out, err := docker.Run(dockerCmdTimeout, "rmi", "-f", ref)
		require.NoErrorf(t, err, "docker rmi: %s", out)

		out, err = docker.Run(dockerCmdTimeout, "pull", ref)
		require.NoErrorf(t, err, "docker pull: %s", out)
	})
}
