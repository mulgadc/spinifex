package service

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/viperblock"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/nats"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/predastore"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/spinifexui"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/sqsstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/vpcd"
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
			svc, err = New(s, &viperblock.Config{})
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

	svc, err := New("sqs-store", &sqsstore.Config{ConfigFile: "/etc/spinifex/sqs-store/sqs-store.conf"})
	assert.NoError(t, err)
	assert.NotNil(t, svc)

	// Test unknown service type
	svc, err = New("unknownservice", nil)
	assert.Error(t, err)
	assert.Nil(t, svc)
}
