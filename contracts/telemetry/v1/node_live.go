package telemetryv1

import "regexp"

// Node live subjects mirror the instance ones, keyed by node name. Only the
// super-admin account's gateway handler subscribes to them.
const (
	nodeLiveWatchSubjectPrefix  = "telemetry.watch.node."
	nodeLiveSampleSubjectPrefix = "telemetry.live.node."
)

var liveNodePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidLiveNode reports whether a node name is safe to place in a subject.
func ValidLiveNode(node string) bool {
	return liveNodePattern.MatchString(node)
}

// NodeLiveWatchSubject is where a viewer asks a node to keep sampling itself.
// A request on it is answered with a NodeLiveRing.
func NodeLiveWatchSubject(node string) string {
	return nodeLiveWatchSubjectPrefix + node
}

// NodeLiveSampleSubject carries one NodeLiveSample per period while watched.
func NodeLiveSampleSubject(node string) string {
	return nodeLiveSampleSubjectPrefix + node
}

// NodeLiveRing answers a watch with the samples newer than its SinceMillis.
type NodeLiveRing struct {
	Samples []NodeLiveSample `json:"samples"`
}

// NodeLiveSample is one host measurement. Rates are per second over the
// interval ending at TimeMillis.
type NodeLiveSample struct {
	TimeMillis int64   `json:"t"`
	CPUPercent float64 `json:"cpu"`
	// MemoryUsedPercent is used memory less page cache, as free(1) reports it.
	MemoryUsedPercent float64 `json:"mem"`
	Load1             float64 `json:"load1"`
	// Planes holds the traffic of each plane's physical NICs, keyed wan, lan, vpc.
	Planes map[string]NodeLivePlane `json:"planes"`
}

// NodeLivePlane is one plane's traffic from the host's side.
type NodeLivePlane struct {
	NetworkIn  float64 `json:"netIn"`
	NetworkOut float64 `json:"netOut"`
	PacketsIn  float64 `json:"pktIn"`
	PacketsOut float64 `json:"pktOut"`
}
