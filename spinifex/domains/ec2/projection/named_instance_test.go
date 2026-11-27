package projection

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidInstanceID(t *testing.T) {
	for _, id := range []string{"i-01234567", "i-0123456789abcdef0"} {
		assert.True(t, ValidInstanceID(id), id)
	}
	for _, id := range []string{"", "i-", "i-0123456", "i-0123456789abcdef", "i-0123456789ABCDEF0", "i-0123456*", "i-*", "i->", "i-01234567.x", "x-01234567", "vol-01234567", "i-0123 4567", "i-01234567\n"} {
		assert.False(t, ValidInstanceID(id), "%q", id)
	}
}

func TestNamedInstance_Running(t *testing.T) {
	assert.True(t, (&NamedInstance{State: "running"}).Running())
	for _, state := range []string{"", "pending", "stopping", "stopped", "shutting-down", "terminated"} {
		assert.False(t, (&NamedInstance{State: state}).Running(), "%q", state)
	}
}
