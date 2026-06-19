package northstar

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRejectsWrongConfigType(t *testing.T) {
	_, err := New(struct{}{})
	require.Error(t, err)
}

func TestNewAcceptsConfig(t *testing.T) {
	svc, err := New(&Config{ConfigPath: "/etc/spinifex/northstar/northstar.toml"})
	require.NoError(t, err)
	require.NotNil(t, svc)
	assert.Equal(t, "/etc/spinifex/northstar/northstar.toml", svc.Config.ConfigPath)
}

func TestStartRequiresConfigPath(t *testing.T) {
	svc, err := New(&Config{})
	require.NoError(t, err)
	_, err = svc.Start()
	require.Error(t, err)
}

func TestReloadNilServerIsNoop(t *testing.T) {
	svc, err := New(&Config{})
	require.NoError(t, err)
	require.NoError(t, svc.Reload())
}
