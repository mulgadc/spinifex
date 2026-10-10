package networkv1_test

import (
	"testing"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	"github.com/stretchr/testify/assert"
)

// TestQueueGroup pins the queue group literal every vpcd subscribes under.
func TestQueueGroup(t *testing.T) {
	assert.Equal(t, "vpcd-workers", networkv1.QueueGroup)
}
