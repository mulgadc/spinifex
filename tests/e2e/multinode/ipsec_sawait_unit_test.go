//go:build e2e

package multinode

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// xfrmPolicyPair is `ip xfrm policy list src A dst B dir out` on a node with
// both outbound policies to one peer: its own Geneve (dport) and replies (sport).
const xfrmPolicyPair = `src 192.0.2.2/32 dst 192.0.2.3/32 proto udp dport 6081
	dir out priority 366911 ptype main
	tmpl src 0.0.0.0 dst 0.0.0.0
		proto esp spi 0xc8bd5500 reqid 4 mode transport
src 192.0.2.2/32 dst 192.0.2.3/32 proto udp sport 6081
	dir out priority 366911 ptype main
	tmpl src 0.0.0.0 dst 0.0.0.0
		proto esp spi 0xc0bea65e reqid 3 mode transport
`

const xfrmPolicyReplyOnly = `src 192.0.2.2/32 dst 192.0.2.3/32 proto udp sport 6081
	dir out priority 366911 ptype main
	tmpl src 0.0.0.0 dst 0.0.0.0
		proto esp spi 0xc0bea65e reqid 3 mode transport
`

// xfrmStateReplyOnly is the stuck leg: the reply SA (reqid 3) is up while the
// Geneve SA (reqid 4) is only a larval acquire state with SPI zero.
const xfrmStateReplyOnly = `src 192.0.2.2 dst 192.0.2.3
	proto esp spi 0xc0bea65e reqid 3 mode transport
	replay-window 0
	aead rfc4106(gcm(aes)) <<Keys hidden>> 128
	dir out
	sel src 192.0.2.2/32 dst 192.0.2.3/32 proto udp sport 6081
src 192.0.2.2 dst 192.0.2.3
	proto esp spi 0x00000000 reqid 4 mode transport
	replay-window 0
	sel src 192.0.2.2/32 dst 192.0.2.3/32 proto udp sport 38675 dport 6081
`

const xfrmStateGeneveUp = `src 192.0.2.2 dst 192.0.2.3
	proto esp spi 0xc8bd5500 reqid 4 mode transport
	replay-window 0
	aead rfc4106(gcm(aes)) <<Keys hidden>> 128
	dir out
	sel src 192.0.2.2/32 dst 192.0.2.3/32 proto udp dport 6081
`

func TestGeneveOutReqid(t *testing.T) {
	reqid, ok := geneveOutReqid(xfrmPolicyPair, "192.0.2.2", "192.0.2.3")
	assert.True(t, ok)
	assert.Equal(t, "4", reqid, "the dport 6081 policy carries this node's Geneve, not the sport one")

	_, ok = geneveOutReqid(xfrmPolicyReplyOnly, "192.0.2.2", "192.0.2.3")
	assert.False(t, ok, "a reply-only policy is not a Geneve policy")

	_, ok = geneveOutReqid(xfrmPolicyPair, "192.0.2.2", "192.0.2.9")
	assert.False(t, ok, "a policy to another peer must not match")

	_, ok = geneveOutReqid("", "192.0.2.2", "192.0.2.3")
	assert.False(t, ok)
}

func TestHasLiveSA(t *testing.T) {
	assert.False(t, hasLiveSA(xfrmStateReplyOnly, "192.0.2.2", "192.0.2.3", "4"),
		"a live reply SA plus a larval Geneve state is the stuck leg, not a ready one")
	assert.True(t, hasLiveSA(xfrmStateReplyOnly, "192.0.2.2", "192.0.2.3", "3"))
	assert.True(t, hasLiveSA(xfrmStateGeneveUp, "192.0.2.2", "192.0.2.3", "4"))
	assert.False(t, hasLiveSA(xfrmStateGeneveUp, "192.0.2.3", "192.0.2.2", "4"), "direction matters")
	assert.False(t, hasLiveSA("", "192.0.2.2", "192.0.2.3", "4"))
}

// fakeClock advances only when the code under test pauses or a probe costs time.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

type testLeg string

func (l testLeg) String() string { return string(l) }

func TestWaitSAsReadyAtOnce(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	triggered := 0
	res := waitSAs([]testLeg{"a", "b"}, time.Minute,
		func(testLeg, time.Duration) (bool, error) { return true, nil },
		func(testLeg, time.Duration) { triggered++ },
		clk.Now, clk.Advance)
	assert.True(t, res.ready)
	assert.Zero(t, res.elapsed)
	assert.Zero(t, triggered, "nothing to trigger when every leg is up")
}

func TestWaitSAsTriggersOnlyMissingLegUntilReady(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	upAt := clk.now.Add(165 * time.Second)
	var triggered []testLeg
	res := waitSAs([]testLeg{"up", "stuck"}, ipsecSAReadyTimeout,
		func(l testLeg, _ time.Duration) (bool, error) { return l == "up" || !clk.now.Before(upAt), nil },
		func(l testLeg, _ time.Duration) { triggered = append(triggered, l) },
		clk.Now, clk.Advance)
	assert.True(t, res.ready)
	assert.Equal(t, 166*time.Second, res.elapsed, "ready at the first poll after the acquire state expires")
	assert.NotEmpty(t, triggered)
	assert.NotContains(t, triggered, testLeg("up"), "a leg that is up must not be poked")
}

func TestWaitSAsExpiresNamingMissingLeg(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	res := waitSAs([]testLeg{"up", "stuck", "unreadable"}, 10*time.Second,
		func(l testLeg, limit time.Duration) (bool, error) {
			assert.LessOrEqual(t, limit, 10*time.Second)
			if l == "unreadable" {
				return false, errors.New("ssh: connection refused")
			}
			return l == "up", nil
		},
		func(testLeg, time.Duration) {},
		clk.Now, clk.Advance)
	assert.False(t, res.ready)
	assert.Equal(t, []string{"stuck", "unreadable (unverified: ssh: connection refused)"}, res.missing,
		"a leg whose state could not be read is not reported as an observed missing SA")
	assert.Equal(t, 10*time.Second, res.elapsed, "the pause is capped at the time left")
}

func TestWaitSAsStartsNothingAfterExpiry(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	var probes, triggers int
	res := waitSAs([]testLeg{"a", "b"}, 10*time.Second,
		func(_ testLeg, limit time.Duration) (bool, error) {
			probes++
			clk.Advance(limit) // each probe uses all the time it is given
			return false, nil
		},
		func(testLeg, time.Duration) { triggers++ },
		clk.Now, clk.Advance)
	assert.False(t, res.ready)
	assert.Equal(t, 1, probes, "the second leg's probe would start after the deadline")
	assert.Zero(t, triggers, "no trigger once the budget is spent")
	assert.Equal(t, 10*time.Second, res.elapsed)
}

func TestWaitSAsRejectsLateReadiness(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	res := waitSAs([]testLeg{"a"}, 10*time.Second,
		func(_ testLeg, limit time.Duration) (bool, error) {
			clk.Advance(limit + time.Second) // SSH overran its context
			return true, nil
		},
		func(testLeg, time.Duration) {},
		clk.Now, clk.Advance)
	assert.False(t, res.ready, "readiness observed after the deadline does not count")
	assert.NotEmpty(t, res.missing)
}

func TestPingUntilExcludesFirstFailHook(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	calls := 0
	res := pingUntil(45*time.Second, func(time.Duration) (string, bool) {
		calls++
		clk.Advance(3 * time.Second)
		return "out", calls == 3
	}, func() { clk.Advance(60 * time.Second) }, clk.Now, clk.Advance)
	assert.True(t, res.converged, "a 60 s diagnostic must not eat the 45 s datapath budget")
	assert.Equal(t, 3, res.attempts)
	assert.Equal(t, 13*time.Second, res.elapsed, "3 attempts of 3 s and 2 pauses of 2 s, without the hook")
}

func TestPingUntilRejectsLateSuccess(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	var lefts []time.Duration
	res := pingUntil(10*time.Second, func(left time.Duration) (string, bool) {
		lefts = append(lefts, left)
		if len(lefts) == 1 {
			clk.Advance(7 * time.Second)
			return "loss", false
		}
		clk.Advance(5 * time.Second)
		return "ok", true
	}, nil, clk.Now, clk.Advance)
	assert.False(t, res.converged, "a success that completes after the deadline does not count")
	assert.Equal(t, []time.Duration{10 * time.Second, time.Second}, lefts, "each attempt gets only the time left")
	assert.Equal(t, 2, res.attempts, "no attempt starts once the budget is spent")
}
