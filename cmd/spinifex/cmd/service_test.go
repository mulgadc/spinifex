package cmd

import (
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/services/northstar"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubNorthstarService struct {
	starts   int
	startErr error
}

func (s *stubNorthstarService) Start() (int, error) {
	s.starts++
	return 0, s.startErr
}

func TestRunNorthstarStartActivation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		nodePath       string
		override       string
		wantPath       string
		wantConfigured bool
	}{
		{
			name: "unconfigured node is skipped",
		},
		{
			name:           "node config starts service",
			nodePath:       "/etc/spinifex/northstar/northstar.toml",
			wantPath:       "/etc/spinifex/northstar/northstar.toml",
			wantConfigured: true,
		},
		{
			name:           "override starts service",
			override:       "/run/spinifex/northstar.toml",
			wantPath:       "/run/spinifex/northstar.toml",
			wantConfigured: true,
		},
		{
			name:           "override takes precedence",
			nodePath:       "/etc/spinifex/northstar/northstar.toml",
			override:       "/run/spinifex/northstar.toml",
			wantPath:       "/run/spinifex/northstar.toml",
			wantConfigured: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			clusterConfig := &config.ClusterConfig{
				Node: "node1",
				Nodes: map[string]config.Config{
					"node1": {
						BaseDir:   "/var/lib/spinifex/northstar",
						Northstar: config.NorthstarConfig{ConfigPath: tt.nodePath},
					},
				},
			}
			stub := &stubNorthstarService{}
			bootstrapCalls := 0
			factoryCalls := 0
			var startedConfig *northstar.Config
			deps := northstarStartDependencies{
				loadConfig: func(string) (*config.ClusterConfig, error) {
					return clusterConfig, nil
				},
				bootstrapBaseZone: func(path string, cfg *config.ClusterConfig) error {
					bootstrapCalls++
					assert.Equal(t, tt.wantPath, path)
					assert.Same(t, clusterConfig, cfg)
					return nil
				},
				newService: func(cfg *northstar.Config) (northstarStarter, error) {
					factoryCalls++
					startedConfig = cfg
					return stub, nil
				},
			}

			err := runNorthstarStart(northstarStartOptions{
				configFile:     "/etc/spinifex/spinifex.toml",
				configOverride: tt.override,
			}, deps)
			require.NoError(t, err)

			if !tt.wantConfigured {
				assert.Zero(t, bootstrapCalls)
				assert.Zero(t, factoryCalls)
				assert.Zero(t, stub.starts)
				assert.Nil(t, startedConfig)
				return
			}
			assert.Equal(t, 1, bootstrapCalls)
			assert.Equal(t, 1, factoryCalls)
			assert.Equal(t, 1, stub.starts)
			require.NotNil(t, startedConfig)
			assert.Equal(t, tt.wantPath, startedConfig.ConfigPath)
		})
	}
}

func TestLoadRequiredClusterConfigRejectsMissingFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
	}{
		{name: "empty path"},
		{name: "missing path", path: t.TempDir() + "/missing.toml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := loadRequiredClusterConfig(tt.path)
			require.Error(t, err)
		})
	}
}

func TestRunNorthstarStartReturnsLoadFailure(t *testing.T) {
	t.Parallel()

	loadErr := errors.New("cluster config unavailable")
	deps := northstarStartDependencies{
		loadConfig: func(string) (*config.ClusterConfig, error) {
			return nil, loadErr
		},
	}

	err := runNorthstarStart(northstarStartOptions{configFile: "/etc/spinifex/spinifex.toml"}, deps)
	require.ErrorIs(t, err, loadErr)
}

func TestRunNorthstarStartContinuesAfterBootstrapFailure(t *testing.T) {
	t.Parallel()

	bootstrapErr := errors.New("predastore unavailable")
	stub := &stubNorthstarService{}
	deps := northstarStartDependencies{
		loadConfig: func(string) (*config.ClusterConfig, error) {
			return &config.ClusterConfig{
				Node: "node1",
				Nodes: map[string]config.Config{
					"node1": {Northstar: config.NorthstarConfig{ConfigPath: "/etc/spinifex/northstar/northstar.toml"}},
				},
			}, nil
		},
		bootstrapBaseZone: func(string, *config.ClusterConfig) error { return bootstrapErr },
		newService: func(*northstar.Config) (northstarStarter, error) {
			return stub, nil
		},
	}

	err := runNorthstarStart(northstarStartOptions{configFile: "/etc/spinifex/spinifex.toml"}, deps)
	require.NoError(t, err)
	assert.Equal(t, 1, stub.starts)
}

func TestRunNorthstarStartRejectsMissingNode(t *testing.T) {
	t.Parallel()

	deps := northstarStartDependencies{
		loadConfig: func(string) (*config.ClusterConfig, error) {
			return &config.ClusterConfig{Node: "missing", Nodes: map[string]config.Config{}}, nil
		},
	}

	err := runNorthstarStart(northstarStartOptions{configFile: "/etc/spinifex/spinifex.toml"}, deps)
	require.EqualError(t, err, `node "missing" not found in cluster config`)
}

func TestRunNorthstarStartReturnsConfiguredFailure(t *testing.T) {
	t.Parallel()

	startErr := errors.New("configured startup failed")
	stub := &stubNorthstarService{startErr: startErr}
	deps := northstarStartDependencies{
		loadConfig: func(string) (*config.ClusterConfig, error) {
			return &config.ClusterConfig{
				Node: "node1",
				Nodes: map[string]config.Config{
					"node1": {Northstar: config.NorthstarConfig{ConfigPath: "/etc/spinifex/northstar/northstar.toml"}},
				},
			}, nil
		},
		bootstrapBaseZone: func(string, *config.ClusterConfig) error { return nil },
		newService: func(*northstar.Config) (northstarStarter, error) {
			return stub, nil
		},
	}

	err := runNorthstarStart(northstarStartOptions{configFile: "/etc/spinifex/spinifex.toml"}, deps)
	require.ErrorIs(t, err, startErr)
	assert.Equal(t, 1, stub.starts)
}
