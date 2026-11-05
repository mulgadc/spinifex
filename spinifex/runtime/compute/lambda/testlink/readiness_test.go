//go:build lambdatestlink

package testlink

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	readyAccount = "000000000001"
	readyRegion  = "ap-southeast-2"
)

// storedFunction creates a function through the real owner and records its
// code as stored, leaving it Pending the way CreateFunction does.
func storedFunction(t *testing.T, svc *function.MetaService, name string) {
	t.Helper()
	storedFunctionWith(t, svc, name, "", 9)
}

func storedFunctionWith(t *testing.T, svc *function.MetaService, name, envCipher string, size int64) {
	t.Helper()
	created, err := svc.Create(t.Context(), &function.CreateRequest{Region: readyRegion, Record: function.Record{
		FunctionID: "id-" + name, FunctionName: name, Role: "arn:aws:iam::000000000001:role/r",
		Runtime: "provided.al2023", Handler: "bootstrap", Timeout: 3, MemorySize: 128, EnvCipher: envCipher,
		Code: function.Code{RevisionID: "rev-" + name},
	}}, readyAccount)
	require.NoError(t, err)
	_, err = svc.CodeStored(t.Context(), &function.CodeStoredRequest{Region: readyRegion, FunctionName: name,
		Revision: created.Revision, SHA256: testImageDigest, Size: size}, readyAccount)
	require.NoError(t, err)
}

func functionState(t *testing.T, svc *function.MetaService, name string) function.Record {
	t.Helper()
	got, err := svc.Get(t.Context(), &function.GetRequest{Region: readyRegion, FunctionName: name}, readyAccount)
	require.NoError(t, err)
	require.True(t, got.Found)
	return got.Record
}

func readyOwner(t *testing.T) *function.MetaService {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	return function.NewMetaService(testutil.NewJetStream(t, nc))
}

func TestReadiness_ActivatesOnlyOnceTheDaemonServesThePinnedImage(t *testing.T) {
	svc := readyOwner(t)
	im, err := PrepareImage(writeImage(t), testImageDigest)
	require.NoError(t, err)
	t.Cleanup(im.Close)
	d := &Daemon{Node: "node1", Image: im}
	svc.SetReadiness(d.Readiness(testImageDigest, 1))
	storedFunction(t, svc, "hello")

	require.NoError(t, svc.ActivatePending(t.Context()))
	waiting := functionState(t, svc, "hello")
	assert.Equal(t, function.StatePending, waiting.State, "a prepared image that is not served activates nothing")
	assert.Equal(t, function.ReasonCreating, waiting.StateReason)

	ctx, cancel := context.WithCancel(t.Context())
	require.NoError(t, d.ServeControl(ctx, controlName(t), os.Getuid(), 1<<20))
	require.NoError(t, svc.ActivatePending(t.Context()))
	active := functionState(t, svc, "hello")
	assert.Equal(t, function.StateActive, active.State)
	assert.Empty(t, active.StateReason)

	cancel()
	require.Eventually(t, func() bool { return !d.Serving() }, 5*time.Second, 10*time.Millisecond,
		"a stopped control socket is reported as not serving")
	storedFunction(t, svc, "later")
	require.NoError(t, svc.ActivatePending(t.Context()))
	assert.Equal(t, function.StatePending, functionState(t, svc, "later").State)
}

func TestReadiness_RefusesAnotherPinOrAnotherNode(t *testing.T) {
	im, err := PrepareImage(writeImage(t), testImageDigest)
	require.NoError(t, err)
	t.Cleanup(im.Close)
	d := &Daemon{Node: "node1", Image: im}
	require.NoError(t, d.ServeControl(t.Context(), controlName(t), os.Getuid(), 1<<20))

	for _, tc := range []struct {
		name   string
		pin    string
		nodes  int
		reason string
	}{
		{"other pin", "0000000000000000000000000000000000000000000000000000000000000000", 1, function.ReasonCreating},
		{"two nodes", testImageDigest, 2, "The function cannot run here: test-link Lambda activates only on a single-node cluster."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := readyOwner(t)
			svc.SetReadiness(d.Readiness(tc.pin, tc.nodes))
			storedFunction(t, svc, "hello")
			require.NoError(t, svc.ActivatePending(t.Context()))
			got := functionState(t, svc, "hello")
			assert.Equal(t, function.StatePending, got.State)
			assert.Equal(t, tc.reason, got.StateReason)
		})
	}
}

func TestReadiness_RefusesFunctionsTheTestLinkCannotRun(t *testing.T) {
	im, err := PrepareImage(writeImage(t), testImageDigest)
	require.NoError(t, err)
	t.Cleanup(im.Close)
	d := &Daemon{Node: "node1", Image: im}
	require.NoError(t, d.ServeControl(t.Context(), controlName(t), os.Getuid(), 1<<20))
	svc := readyOwner(t)
	svc.SetReadiness(d.Readiness(testImageDigest, 1))
	storedFunctionWith(t, svc, "env", "sealed", 9)
	storedFunctionWith(t, svc, "huge", "", MaxCodeBytes+1)
	storedFunctionWith(t, svc, "edge", "", MaxCodeBytes)

	require.NoError(t, svc.ActivatePending(t.Context()))
	assert.Equal(t, "The function cannot run here: test-link Lambda does not support environment variables.", functionState(t, svc, "env").StateReason)
	assert.Equal(t, function.StatePending, functionState(t, svc, "env").State)
	assert.Equal(t, "The function cannot run here: its code is larger than test-link Lambda accepts.", functionState(t, svc, "huge").StateReason)
	assert.Equal(t, function.StatePending, functionState(t, svc, "huge").State)
	assert.Equal(t, function.StateActive, functionState(t, svc, "edge").State, "the largest accepted code copy activates")
}
