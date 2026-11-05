//go:build !lambdatestlink

package guestconn

import (
	"testing"

	lambdav1 "github.com/mulgadc/spinifex/contracts/lambda/v1"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/stretchr/testify/assert"
)

// Release builds keep the published protocol on both sides.
func TestReleaseBuildOffersALPNv1(t *testing.T) {
	assert.Equal(t, []string{lambdav1.ALPNv1}, newGeneration(t, gen1).ServerConfig().NextProtos)
	assert.Equal(t, lambdav1.ALPNv1, hostlink.Protocol)
}
