package vm

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func consoleFixture(t *testing.T, id string) (*VM, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "console-"+id+".log")
	require.NoError(t, os.WriteFile(path, []byte("last output\n"), 0o644))
	return &VM{
		ID:           id,
		Status:       StateRunning,
		InstanceType: "t3.micro",
		Instance:     &ec2.Instance{},
		Config:       Config{ConsoleLogPath: path},
	}, path
}

func requireRetired(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist, "the shipper's console-*.log glob must no longer match")
	got, err := os.ReadFile(RetiredConsolePath(path))
	require.NoError(t, err)
	assert.Equal(t, "last output\n", string(got), "the content survives the rename")
}

func requireLive(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	require.NoError(t, err)
	_, err = os.Stat(RetiredConsolePath(path))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestTerminate_RetiresConsoleLog(t *testing.T) {
	m, _, _, _, _ := shutdownTestManager(t)
	instance, path := consoleFixture(t, "i-term-console")
	m.Insert(instance)

	require.NoError(t, m.Terminate(instance.ID))

	requireRetired(t, path)
}

func TestMarkFailed_RetiresConsoleLog(t *testing.T) {
	m, _, _, _, _ := shutdownTestManager(t)
	instance, path := consoleFixture(t, "i-failed-console")
	instance.FirstLaunch = true
	instance.Status = StatePending
	m.Insert(instance)

	m.MarkFailed(context.Background(), instance, "launch failed")
	m.goroutineWg.Wait()

	requireRetired(t, path)
}

func TestStop_KeepsConsoleLog(t *testing.T) {
	m, _, _, _, _ := shutdownTestManager(t)
	instance, path := consoleFixture(t, "i-stop-console")
	m.Insert(instance)

	require.NoError(t, m.Stop(instance.ID))

	requireLive(t, path)
}

func TestStopAll_DrainKeepsConsoleLog(t *testing.T) {
	m, _, _, _, _ := shutdownTestManager(t)
	instance, path := consoleFixture(t, "i-drain-console")
	m.Insert(instance)

	require.NoError(t, m.StopAll())

	requireLive(t, path)
}
