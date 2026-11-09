package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/mulgadc/spinifex/spinifex/runtime/daemon"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/cobra"
)

var clusterStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the cluster feature level and each member's state",
	Long: `status prints the recorded cluster feature level, any hold and any rollback in
progress, then each configured member's reported level, heartbeat age, and its
answer to the status request (or the reason it gave none). It only reads.`,
	Run: runClusterStatus,
}

func init() {
	clusterCmd.AddCommand(clusterStatusCmd)
}

// memberLevelReport is what status shows for one configured member.
type memberLevelReport struct {
	Node          string
	HasHeartbeat  bool
	HeartbeatNote string // why there is no usable heartbeat
	BeatLevel     int
	BeatAge       time.Duration
	Status        featurelevel.NodeStatus
}

func runClusterStatus(_ *cobra.Command, _ []string) {
	cfg, nc, err := loadConfigAndConnect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer nc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	jsm, err := daemon.NewJetStreamManager(nc)
	if err == nil {
		err = jsm.InitClusterConfigBucket()
	}
	if err == nil {
		err = jsm.InitClusterStateBucket()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	store, err := jsm.FeatureLevelStore(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	rec, rev, err := store.Read(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	nodes := slices.Sorted(maps.Keys(cfg.Nodes))
	statuses := featurelevel.NewProbe(nc, nodes).Probe(ctx)
	now := time.Now()
	members := make([]memberLevelReport, len(nodes))
	for i, node := range nodes {
		members[i] = memberLevelReport{Node: node, Status: statuses[i]}
		h, herr := jsm.ReadHeartbeat(node)
		switch {
		case errors.Is(herr, jetstream.ErrKeyNotFound):
			members[i].HeartbeatNote = "no heartbeat"
		case herr != nil:
			members[i].HeartbeatNote = "heartbeat unreadable: " + herr.Error()
		default:
			written, perr := time.Parse(time.RFC3339, h.Timestamp)
			if perr != nil {
				members[i].HeartbeatNote = "heartbeat timestamp unreadable"
				break
			}
			members[i].HasHeartbeat = true
			members[i].BeatLevel = h.FeatureLevel
			members[i].BeatAge = now.Sub(written)
		}
	}
	renderLevelStatus(os.Stdout, rec, rev, featurelevel.Build, members)
}

// renderLevelStatus prints the record, then one line per member.
func renderLevelStatus(w io.Writer, rec featurelevel.Record, rev uint64, build int, members []memberLevelReport) {
	fmt.Fprintf(w, "Feature level: %d (this build supports %d, record revision %d)\n", rec.Level, build, rev)
	if rec.Hold.Held {
		fmt.Fprintf(w, "Hold:          held at %d (release with: spx admin cluster release-hold)\n", rec.Hold.Level)
	} else {
		fmt.Fprintln(w, "Hold:          none")
	}
	if rec.Rollback != nil {
		fmt.Fprintf(w, "Rollback:      to %d in progress\n", rec.Rollback.Target)
	} else {
		fmt.Fprintln(w, "Rollback:      none")
	}
	fmt.Fprintln(w, "Members:")
	for _, m := range members {
		fmt.Fprintf(w, "  %s: %s; %s\n", m.Node, heartbeatText(m), statusText(m.Status))
	}
}

func heartbeatText(m memberLevelReport) string {
	if !m.HasHeartbeat {
		return m.HeartbeatNote
	}
	return fmt.Sprintf("heartbeat level %d, age %s", m.BeatLevel, m.BeatAge.Round(time.Second))
}

func statusText(s featurelevel.NodeStatus) string {
	if s.Reply == nil {
		return "status: none (" + s.Reason + ")"
	}
	return fmt.Sprintf("status: build %d, revision %d, in flight %d", s.Reply.Build, s.Reply.Revision, s.Reply.InFlightTotal)
}
