package tagging_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mulgadc/spinifex/spinifex/domains/tagging"
)

func TestMatchesType(t *testing.T) {
	// Empty filter set admits everything.
	assert.True(t, tagging.MatchesType(nil, "elasticloadbalancing:loadbalancer"))

	exact := map[string]bool{"elasticloadbalancing:loadbalancer": true}
	assert.True(t, tagging.MatchesType(exact, "elasticloadbalancing:loadbalancer"))
	assert.False(t, tagging.MatchesType(exact, "elasticloadbalancing:targetgroup"))
	assert.False(t, tagging.MatchesType(exact, "ec2:subnet"))

	// Service-only filter admits all of that service's types.
	svcOnly := map[string]bool{"ec2": true}
	assert.True(t, tagging.MatchesType(svcOnly, "ec2:subnet"))
	assert.True(t, tagging.MatchesType(svcOnly, "ec2:security-group"))
	assert.False(t, tagging.MatchesType(svcOnly, "elasticloadbalancing:loadbalancer"))
}
