//go:build e2e

package fault

import "time"

// LinkProfile describes a tc netem configuration approximating a real-world
// tactical link. Values are unvalidated approximations derived from public
// specifications; see docs/development/improvements/ddil-e2e-test-harness.md
// §2 and spinifex/tests/e2e/TEST_COVERAGE.md for validation tracking.
type LinkProfile struct {
	Name      string
	Delay     time.Duration
	Jitter    time.Duration
	Loss      float64 // percent, 0..100
	Bandwidth string  // tc-compatible rate, e.g. "512kbit"; empty means unshaped
	Flapping  *FlapSpec
}

// FlapSpec toggles link up/down for profiles that simulate jammed or
// intermittent RF. The flap loop itself is not driven by ApplyNetem — it is
// orchestrated by a higher-level helper (added with the scenarios that use
// it) so scenarios keep full control over when the flap stops.
type FlapSpec struct {
	Up   time.Duration
	Down time.Duration
}

// Authoritative DDIL link profile values. See the §2 table in the design doc
// for the rationale behind each row. Replace with measured values when
// real-hardware access is available.
var (
	LAN = LinkProfile{
		Name:      "LAN",
		Delay:     1 * time.Millisecond,
		Jitter:    0,
		Loss:      0,
		Bandwidth: "1gbit",
	}
	WAN = LinkProfile{
		Name:      "WAN",
		Delay:     50 * time.Millisecond,
		Jitter:    10 * time.Millisecond,
		Loss:      0.1,
		Bandwidth: "100mbit",
	}
	LTEDegraded = LinkProfile{
		Name:      "LTEDegraded",
		Delay:     100 * time.Millisecond,
		Jitter:    30 * time.Millisecond,
		Loss:      5,
		Bandwidth: "1mbit",
	}
	SATCOM = LinkProfile{
		Name:      "SATCOM",
		Delay:     600 * time.Millisecond,
		Jitter:    50 * time.Millisecond,
		Loss:      2,
		Bandwidth: "512kbit",
	}
	HFData = LinkProfile{
		Name:      "HFData",
		Delay:     2000 * time.Millisecond,
		Jitter:    200 * time.Millisecond,
		Loss:      10,
		Bandwidth: "9600bit",
	}
	Flapping = LinkProfile{
		Name: "Flapping",
		Flapping: &FlapSpec{
			Up:   10 * time.Second,
			Down: 5 * time.Second,
		},
	}
)

// AllProfiles lists every predefined profile. Useful for doc-drift checks and
// for the TEST_COVERAGE.md profile-validation table.
var AllProfiles = []LinkProfile{LAN, WAN, LTEDegraded, SATCOM, HFData, Flapping}
