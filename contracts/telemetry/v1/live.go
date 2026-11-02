package telemetryv1

import (
	"regexp"
)

// LiveSamplePeriodMillis is how often a watched instance is sampled for the
// console's live view.
const LiveSamplePeriodMillis = 500

// LiveWatchLeaseMillis is how long one watch keeps the live sampler running.
// Watchers renew well inside it, so a viewer that goes away costs at most this.
const LiveWatchLeaseMillis = 30_000

// LiveRingSamples bounds the in-memory history a live sampler keeps for a
// reconnecting viewer: five minutes at LiveSamplePeriodMillis.
const LiveRingSamples = 5 * 60 * 1000 / LiveSamplePeriodMillis

// Live subjects sit outside metrics.> so nothing that ingests the periodic
// CloudWatch batches ever reads a sub-second rate as a period. The account is part
// of the subject, so only the poller for that account's instance answers.
const (
	liveWatchSubjectPrefix  = "telemetry.watch.ec2."
	liveSampleSubjectPrefix = "telemetry.live.ec2."
)

var (
	liveAccountPattern  = regexp.MustCompile(`^[0-9]{12}$`)
	liveInstancePattern = regexp.MustCompile(`^i-[0-9a-f]{8,17}$`)
)

// ValidLiveTarget reports whether accountID and instanceID are safe to place
// in a live subject: no NATS token separators or wildcards can get through.
func ValidLiveTarget(accountID, instanceID string) bool {
	return liveAccountPattern.MatchString(accountID) && liveInstancePattern.MatchString(instanceID)
}

// LiveWatchSubject is where a viewer asks the owning node to keep sampling. A
// request on it is answered with the ring since LiveWatch.SinceMillis.
func LiveWatchSubject(accountID, instanceID string) string {
	return liveWatchSubjectPrefix + accountID + "." + instanceID
}

// LiveSampleSubject carries one LiveSample per sampling period while watched.
func LiveSampleSubject(accountID, instanceID string) string {
	return liveSampleSubjectPrefix + accountID + "." + instanceID
}

// LiveWatch is the body of a watch. SinceMillis is the newest sample the
// viewer already has; zero asks for the whole ring.
type LiveWatch struct {
	SinceMillis int64 `json:"since_ms,omitempty"`
}

// LiveRing answers a watch request with the samples newer than SinceMillis.
type LiveRing struct {
	Samples []LiveSample `json:"samples"`
}

// LiveSample is one live measurement. Every value is a rate over the interval
// ending at TimeMillis, measured against the previous sample's clock.
type LiveSample struct {
	TimeMillis int64   `json:"t"`
	CPUPercent float64 `json:"cpu"`
	// NetworkIn and NetworkOut are bytes per second from the guest's side.
	NetworkIn  float64 `json:"netIn"`
	NetworkOut float64 `json:"netOut"`
	// PacketsIn and PacketsOut are packets per second from the guest's side.
	PacketsIn  float64 `json:"pktIn"`
	PacketsOut float64 `json:"pktOut"`
}
