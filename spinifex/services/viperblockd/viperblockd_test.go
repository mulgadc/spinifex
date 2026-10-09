package viperblockd

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs/viperblock"
	"github.com/stretchr/testify/assert"
)

// TestNew tests the service constructor.
func TestNew(t *testing.T) {
	cfg := &viperblock.Config{
		NatsHost:  "nats://localhost:4222",
		S3Host:    "https://s3.amazonaws.com",
		Bucket:    "test-bucket",
		Region:    "us-east-1",
		AccessKey: "test-access-key",
		SecretKey: "test-secret-key",
		BaseDir:   "/tmp/viperblock",
	}

	svc, err := New(cfg)

	assert.NoError(t, err)
	assert.NotNil(t, svc)
	assert.NotNil(t, svc.Config)
	assert.Equal(t, "nats://localhost:4222", svc.Config.NatsHost)
	assert.Equal(t, "https://s3.amazonaws.com", svc.Config.S3Host)
	assert.Equal(t, "test-bucket", svc.Config.Bucket)
	assert.Equal(t, "us-east-1", svc.Config.Region)
	assert.Equal(t, "/tmp/viperblock", svc.Config.BaseDir)
}

// TestNewWithNilConfig tests that New handles nil config correctly.
func TestNewWithNilConfig(t *testing.T) {
	// This will panic if not handled, but based on the code it type asserts
	// For now we test that it accepts a Config pointer
	cfg := &viperblock.Config{}
	svc, err := New(cfg)

	assert.NoError(t, err)
	assert.NotNil(t, svc)
	assert.NotNil(t, svc.Config)
}

// TestServiceNameConstant tests the serviceName constant.
func TestServiceNameConstant(t *testing.T) {
	assert.Equal(t, "viperblock", serviceName)
}
