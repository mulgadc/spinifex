package cmd_test

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/mulgadc/spinifex/cmd/spinifex/cmd"
	"github.com/mulgadc/spinifex/spinifex/accountteardown"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureOutput redirects os.Stdout for the duration of fn and returns what was
// written. The teardown printers are the operator's only view of what is about
// to be destroyed, so what they print is the thing under test.
func captureOutput(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stdout
	os.Stdout = w //nolint:reassign // test-local stdout capture, restored below

	fn()

	require.NoError(t, w.Close())
	os.Stdout = orig //nolint:reassign // restoring the real os.Stdout captured above

	var buf bytes.Buffer
	_, err = io.Copy(&buf, r)
	require.NoError(t, err)
	return buf.String()
}

// feedStdin swaps os.Stdin for a pipe carrying content, mirroring the stdout
// capture above.
func feedStdin(t *testing.T, content string, fn func()) {
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

func teardownPlan() *accountteardown.Result {
	return &accountteardown.Result{
		AccountID:   "000000000042",
		AccountName: "tenant@example.com",
		DryRun:      true,
		Stages: []accountteardown.StageResult{
			{
				Stage:   accountteardown.StageCompute,
				Deleted: []accountteardown.Resource{{Kind: "instance", ID: "i-1", Detail: "running"}},
				Elapsed: "0s",
			},
			{Stage: accountteardown.StageAttachments, Elapsed: "0s"},
			{
				Stage:   accountteardown.StageStorage,
				Deleted: []accountteardown.Resource{{Kind: "volume", ID: "vol-1"}},
				Elapsed: "0s",
			},
		},
	}
}

// The plan is what the operator confirms against, so every resource has to
// appear under its stage — and an empty stage must not pad the list.
func TestPrintTeardownPlanListsEveryResource(t *testing.T) {
	output := captureOutput(t, func() { cmd.PrintTeardownPlan(teardownPlan()) })

	assert.Contains(t, output, "000000000042")
	assert.Contains(t, output, "tenant@example.com")
	assert.Contains(t, output, "i-1")
	assert.Contains(t, output, "vol-1")
	assert.Contains(t, output, "compute")
	assert.Contains(t, output, "storage")
	assert.NotContains(t, output, "attachments", "an empty stage is noise, not information")
}

// An account with nothing in it must say so. A bare header reads as truncated
// output, which is exactly when an operator stops and asks.
func TestPrintTeardownPlanSaysWhenTheAccountIsEmpty(t *testing.T) {
	output := captureOutput(t, func() {
		cmd.PrintTeardownPlan(&accountteardown.Result{
			AccountID:   "000000000042",
			AccountName: "tenant@example.com",
			Stages:      []accountteardown.StageResult{{Stage: accountteardown.StageCompute, Elapsed: "0s"}},
		})
	})

	assert.Contains(t, output, "holds no resources")
}

// A stuck resource is the most valuable thing a teardown reports: each one is a
// delete path that does not work. It must be named, with the reason.
func TestPrintTeardownResultNamesStuckResources(t *testing.T) {
	result := &accountteardown.Result{
		AccountID: "000000000042",
		Stages: []accountteardown.StageResult{
			{
				Stage:   accountteardown.StageCompute,
				Deleted: []accountteardown.Resource{{Kind: "instance", ID: "i-1"}},
				Elapsed: "5s",
			},
			{Stage: accountteardown.StageAttachments, Elapsed: "0s"},
			{
				Stage: accountteardown.StageStorage,
				Stuck: []accountteardown.Stuck{{
					Resource: accountteardown.Resource{Kind: "volume", ID: "vol-1"},
					Reason:   "still attached to i-1",
				}},
				Elapsed: "5m0s",
			},
		},
	}

	output := captureOutput(t, func() { cmd.PrintTeardownResult(result) })

	assert.Contains(t, output, "1 deleted, 0 stuck")
	assert.Contains(t, output, "STUCK")
	assert.Contains(t, output, "vol-1")
	assert.Contains(t, output, "still attached to i-1")
	assert.NotContains(t, output, "attachments")
}

// The typed name is the confirmation. Surrounding whitespace is a paste
// artefact rather than a mismatch, but an empty line is a cancellation.
func TestPromptAccountNameReadsTheConfirmation(t *testing.T) {
	var name string
	var err error

	captureOutput(t, func() {
		feedStdin(t, "  tenant@example.com  \n", func() {
			name, err = cmd.PromptAccountName("000000000042")
		})
	})

	require.NoError(t, err)
	assert.Equal(t, "tenant@example.com", name)
}

func TestPromptAccountNameTreatsAnEmptyReplyAsCancelled(t *testing.T) {
	var err error

	captureOutput(t, func() {
		feedStdin(t, "\n", func() {
			_, err = cmd.PromptAccountName("000000000042")
		})
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cancelled")
}
