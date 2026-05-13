package service

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/services/nats"
	"github.com/mulgadc/spinifex/spinifex/services/predastore"
	"github.com/mulgadc/spinifex/spinifex/services/spinifexui"
	"github.com/mulgadc/spinifex/spinifex/services/viperblockd"
	"github.com/mulgadc/spinifex/spinifex/services/vpcd"
	"github.com/stretchr/testify/assert"
)

func TestNew(t *testing.T) {
	// Test known service types
	services := []string{"nats", "predastore", "viperblock", "spinifex", "awsgw", "spinifex-ui", "vpcd"}

	for _, s := range services {
		var svc Service
		var err error

		switch s {
		// TODO: Standardize service config handling (use config.Config for all?)
		case "nats":
			svc, err = New(s, &nats.Config{})
		case "predastore":
			svc, err = New(s, &predastore.Config{})
			// No special setup needed
		case "viperblock":
			svc, err = New(s, &viperblockd.Config{})
		case "spinifex":
			svc, err = New(s, &config.ClusterConfig{})
		case "awsgw":
			svc, err = New(s, &config.ClusterConfig{})
			// No special setup needed
		case "spinifex-ui":
			svc, err = New(s, &spinifexui.Config{})
		case "vpcd":
			svc, err = New(s, &vpcd.Config{})
		}

		assert.NoError(t, err)
		assert.NotNil(t, svc)
	}

	// Test unknown service type
	svc, err := New("unknownservice", nil)
	assert.Error(t, err)
	assert.Nil(t, svc)
}
