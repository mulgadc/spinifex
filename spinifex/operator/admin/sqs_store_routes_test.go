package admin

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSQSStoreRoutes(t *testing.T) {
	got := SQSStoreRoutes([]string{"10.0.0.1:4248", " 10.0.0.2:4248", "bad", "[fd00::3]:4248"})
	assert.Equal(t, []string{"10.0.0.1:4249", "10.0.0.2:4249", "[fd00::3]:4249"}, got)
	assert.Empty(t, SQSStoreRoutes(nil))
}
