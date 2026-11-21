package cli

import (
	"testing"

	vbengine "github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
)

func TestResolveMapFormatDefault_OnlyTwoSelectsFormatTwo(t *testing.T) {
	two, one, other := 2, 1, 7
	assert.Equal(t, vbengine.MapFormatLegacy, resolveMapFormatDefault(nil), "an upgraded config without the key stays on format 1")
	assert.Equal(t, vbengine.MapFormatLegacy, resolveMapFormatDefault(&one))
	assert.Equal(t, vbengine.MapFormatLegacy, resolveMapFormatDefault(&other))
	assert.Equal(t, vbengine.MapFormatDelta, resolveMapFormatDefault(&two))
}
