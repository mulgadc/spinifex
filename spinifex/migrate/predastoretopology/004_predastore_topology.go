// Package predastoretopology migrates a Predastore install onto the v5 cluster
// topology: hosts owning a data directory, nodes pinned to them by role.
//
// It sits outside the migrate package because it needs Predastore's cluster
// runtime to rewrite raft state, and migrate is imported by every handler that
// stamps a KV bucket version. Only the spx binary that runs `admin upgrade`
// links this.
package predastoretopology

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/mulgadc/predastore/clusterrun"
	"github.com/mulgadc/spinifex/spinifex/migrate"
)

// The predastore config target, relative to the Spinifex config directory.
const (
	predastoreTarget  = "predastore.toml"
	predastoreRelPath = "predastore/predastore.toml"
	// predastoreClusterVersion is the schema the cluster topology lands on;
	// the install templates stamp it directly.
	predastoreClusterVersion = 5
)

// predastoreHostPort is the port a v5 host binds. Every v4 [[db]] entry used
// it too, so a migrated topology keeps the same firewall surface.
const predastoreHostPort = 6660

// Predastore v5 node roles, mirroring internal/cluster.Role.
const (
	predastoreRoleShard   = "shard-storage"
	predastoreRoleReplica = "state-replica"
)

// predastoreV4Node is the v4 shape of a [[db]] or [[nodes]] entry, reduced to
// the fields the migration needs. A frozen copy keeps the migration
// independent of predastore's config surface, which no longer parses these.
type predastoreV4Node struct {
	ID   int    `toml:"id"`
	Host string `toml:"host"`
	Path string `toml:"path"`
}

// predastoreV4Config is the v4 topology. Only the two tables the migration
// rewrites are declared; every other key rides through the rewrite as text.
type predastoreV4Config struct {
	DB    []predastoreV4Node `toml:"db"`
	Nodes []predastoreV4Node `toml:"nodes"`
}

// predastoreHost is one v5 [[host]]: a predastore process owning a socket and
// a data directory.
type predastoreHost struct {
	ID         int
	BindAddr   string
	PublicAddr string
	DataDir    string
}

// predastoreNode is one v5 [[node]]: a role pinned to a host.
type predastoreNode struct {
	ID     int
	HostID int
	Role   string
}

// predastoreMove relocates one node's state from its v4 directory to the v5
// one derived from the host data directory and the node id.
type predastoreMove struct {
	NodeID int
	Role   string
	From   string
	To     string
}

// predastoreTopology is everything derived from a v4 config: the tables to
// write, the directories to move, and the replica set any relocated state
// replica must be recovered onto.
type predastoreTopology struct {
	Hosts      []predastoreHost
	Nodes      []predastoreNode
	Moves      []predastoreMove
	ReplicaIDs []int
}

// predastoreHostTableRe detects a config that already carries the v5 topology,
// so a part-applied migration can be re-run.
var predastoreHostTableRe = regexp.MustCompile(`(?m)^\s*\[\[host\]\]\s*$`)

// predastoreTopLevelAddrRe matches the v4 gateway address keys, which must go:
// a scalar host cannot coexist with the [[host]] tables.
var predastoreTopLevelAddrRe = regexp.MustCompile(`^(host|port)\s*=`)

// predastoreTableHeaderRe captures the name of a table or table-array header.
var predastoreTableHeaderRe = regexp.MustCompile(`^\[\[?([^\[\]]+)\]?\]$`)

func init() {
	migrate.DefaultRegistry.RegisterConfigTarget(predastoreTarget, predastoreRelPath, &migrate.TOMLVersionReader{})
	migrate.DefaultRegistry.RegisterConfig(predastoreTarget, migrate.ConfigMigration{
		FromVersion: 4,
		ToVersion:   predastoreClusterVersion,
		Description: "predastore [[db]]/[[nodes]] → [[host]]/[[node]]; relocate node data and recover raft",
		Run:         migratePredastoreTopology,
	})
}

// migratePredastoreTopology carries a v4 install onto the v5 cluster topology:
// the data moves first, since a failure there leaves a config still describing
// where the data actually is.
func migratePredastoreTopology(ctx migrate.ConfigContext) error {
	path := filepath.Join(ctx.ConfigDir, predastoreRelPath)

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	if predastoreHostTableRe.Match(data) {
		ctx.Logger.Info("Predastore config already carries [[host]] tables, leaving it alone", "path", path)
		return nil
	}

	var old predastoreV4Config
	if err := toml.Unmarshal(data, &old); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	topo, err := predastoreTopologyFromV4(old, ctx.DataDir)
	if err != nil {
		return err
	}

	if err := relocatePredastoreData(ctx, topo); err != nil {
		return err
	}

	out, err := rewritePredastoreConfig(data, topo)
	if err != nil {
		return err
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.WriteFile(path, out, info.Mode()); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// predastoreTopologyFromV4 derives the v5 topology from a v4 config.
//
// A machine is one host. Shard nodes keep their ids, because object metadata
// records which node holds each shard; the state replicas are the ones that
// move, taking ids above the shard range.
func predastoreTopologyFromV4(old predastoreV4Config, spxRoot string) (*predastoreTopology, error) {
	if len(old.DB) == 0 || len(old.Nodes) == 0 {
		return nil, fmt.Errorf("predastore config has %d [[db]] and %d [[nodes]] entries; not a v4 distributed topology", len(old.DB), len(old.Nodes))
	}

	db := append([]predastoreV4Node(nil), old.DB...)
	shards := append([]predastoreV4Node(nil), old.Nodes...)
	sort.Slice(db, func(i, j int) bool { return db[i].ID < db[j].ID })
	sort.Slice(shards, func(i, j int) bool { return shards[i].ID < shards[j].ID })

	// A single-node install lists every db peer on one address; that is one
	// machine, so one host carries the lot.
	singleMachine := true
	for _, n := range db[1:] {
		if n.Host != db[0].Host {
			singleMachine = false
			break
		}
	}

	dataDir := predastoreDataDir(spxRoot)
	basePath := predastoreBasePath(spxRoot)

	topo := &predastoreTopology{}
	hostByAddr := make(map[string]int, len(db))

	if singleMachine {
		hostByAddr[db[0].Host] = 1
		topo.Hosts = append(topo.Hosts, predastoreHost{
			ID:         1,
			BindAddr:   fmt.Sprintf("%s:%d", db[0].Host, predastoreHostPort),
			PublicAddr: fmt.Sprintf("%s:%d", db[0].Host, predastoreHostPort),
			DataDir:    dataDir,
		})
	} else {
		for _, n := range db {
			if _, dup := hostByAddr[n.Host]; dup {
				return nil, fmt.Errorf("predastore config lists two [[db]] entries on %s; cannot derive one host per machine", n.Host)
			}
			hostByAddr[n.Host] = n.ID
			topo.Hosts = append(topo.Hosts, predastoreHost{
				ID: n.ID,
				// The v4 multi-node template bound every interface; keep that
				// rather than narrowing an operator's reachability on upgrade.
				BindAddr:   fmt.Sprintf("0.0.0.0:%d", predastoreHostPort),
				PublicAddr: fmt.Sprintf("%s:%d", n.Host, predastoreHostPort),
				DataDir:    dataDir,
			})
		}
	}

	for _, n := range shards {
		hostID, ok := hostByAddr[n.Host]
		if !ok {
			return nil, fmt.Errorf("predastore shard node %d is on %s, which has no [[db]] entry to place it", n.ID, n.Host)
		}
		topo.Nodes = append(topo.Nodes, predastoreNode{ID: n.ID, HostID: hostID, Role: predastoreRoleShard})
		topo.Moves = append(topo.Moves, predastoreMove{
			NodeID: n.ID,
			Role:   predastoreRoleShard,
			From:   predastoreV4Path(basePath, n.Path),
			To:     filepath.Join(dataDir, fmt.Sprintf("node-%d", n.ID)),
		})
	}

	// State replica ids start above the shard range so the two never collide.
	offset := shards[len(shards)-1].ID
	for _, n := range db {
		id := n.ID + offset
		hostID := hostByAddr[n.Host]
		topo.Nodes = append(topo.Nodes, predastoreNode{ID: id, HostID: hostID, Role: predastoreRoleReplica})
		topo.ReplicaIDs = append(topo.ReplicaIDs, id)
		topo.Moves = append(topo.Moves, predastoreMove{
			NodeID: id,
			Role:   predastoreRoleReplica,
			From:   predastoreV4Path(basePath, n.Path),
			To:     filepath.Join(dataDir, fmt.Sprintf("node-%d", id)),
		})
	}

	return topo, nil
}

// relocatePredastoreData moves each node's state into the v5 layout and
// rewrites the raft configuration of every state replica it moves.
//
// Only this machine's directories exist, but the config describes the whole
// cluster, so a missing source is the normal multi-node case rather than an
// error. A destination that already exists is a re-run.
func relocatePredastoreData(ctx migrate.ConfigContext, topo *predastoreTopology) error {
	for _, m := range topo.Moves {
		moved, err := movePredastoreNodeDir(ctx, m)
		if err != nil {
			return err
		}
		if !moved || m.Role != predastoreRoleReplica {
			continue
		}

		// The replica set is renumbered, so the configuration persisted here
		// names servers that no longer exist. Raft will not bootstrap over
		// existing state to correct that; it has to be rewritten.
		recovered, err := clusterrun.RecoverStateReplica(m.To, m.NodeID, topo.ReplicaIDs)
		if err != nil {
			return fmt.Errorf("recover raft configuration for state replica %d in %s: %w", m.NodeID, m.To, err)
		}
		ctx.Logger.Info("Migrated predastore state replica", "nodeID", m.NodeID, "dataDir", m.To, "raftRecovered", recovered)
	}
	return nil
}

// movePredastoreNodeDir moves one node directory, reporting whether the
// destination now holds state that came from a v4 layout.
func movePredastoreNodeDir(ctx migrate.ConfigContext, m predastoreMove) (bool, error) {
	if _, err := os.Stat(m.From); err != nil {
		if !os.IsNotExist(err) {
			return false, fmt.Errorf("stat %s: %w", m.From, err)
		}
		// Already moved by an earlier run; the replica still needs recovering
		// if that run failed before it got there.
		if _, err := os.Stat(m.To); err == nil {
			return true, nil
		}
		return false, nil
	}

	if _, err := os.Stat(m.To); err == nil {
		return false, fmt.Errorf("cannot move %s to %s: destination already exists", m.From, m.To)
	}

	if err := os.MkdirAll(filepath.Dir(m.To), 0750); err != nil {
		return false, fmt.Errorf("create %s: %w", filepath.Dir(m.To), err)
	}
	if err := os.Rename(m.From, m.To); err != nil {
		return false, fmt.Errorf("move %s to %s: %w", m.From, m.To, err)
	}
	ctx.Logger.Info("Moved predastore node data", "nodeID", m.NodeID, "role", m.Role, "from", m.From, "to", m.To)
	return true, nil
}

// rewritePredastoreConfig replaces the v4 topology tables and the gateway
// address keys with the v5 tables, leaving every other table as the operator
// left it. Re-rendering from the template would instead discard their buckets,
// credentials and rate limits.
func rewritePredastoreConfig(data []byte, topo *predastoreTopology) ([]byte, error) {
	lines := strings.Split(string(data), "\n")

	var out, pending []string
	var table string
	dropping := false
	wroteTopology := false
	wroteGatewayNote := false

	// flush emits the buffered comment/blank block ahead of a line that keeps it.
	flush := func() {
		out = append(out, pending...)
		pending = nil
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if m := predastoreTableHeaderRe.FindStringSubmatch(trimmed); m != nil {
			table = strings.TrimSpace(m[1])
			// The comment block introducing a dropped table goes with it.
			if table == "db" || table == "nodes" {
				pending = nil
				dropping = true
				if !wroteTopology {
					out = append(out, predastoreTopologyBlock(topo)...)
					wroteTopology = true
				}
				continue
			}
			dropping = false
			flush()
			out = append(out, line)
			continue
		}

		blankOrComment := trimmed == "" || strings.HasPrefix(trimmed, "#")

		if dropping {
			// Buffer separators and trailing comments; a key line means the
			// comments above it described the dropped table, so let them go.
			if blankOrComment {
				pending = append(pending, line)
			} else {
				pending = nil
			}
			continue
		}

		if blankOrComment {
			pending = append(pending, line)
			continue
		}

		if table == "" && predastoreTopLevelAddrRe.MatchString(trimmed) {
			pending = nil
			if !wroteGatewayNote {
				out = append(out, predastoreGatewayNote...)
				wroteGatewayNote = true
			}
			continue
		}

		flush()
		out = append(out, line)
	}
	flush()

	if !wroteTopology {
		return nil, fmt.Errorf("predastore config has no [[db]] or [[nodes]] tables to replace")
	}
	return []byte(strings.Join(out, "\n")), nil
}

// predastoreGatewayNote replaces the top-level host/port keys, which the v5
// schema cannot accept.
var predastoreGatewayNote = []string{
	"",
	"# The gateway listen address comes from the daemon CLI options: a top-level",
	"# host key would collide with the [[host]] topology tables below.",
}

// predastoreTopologyBlock renders the v5 tables, matching the layout the
// install templates generate so a migrated config reads like a fresh one.
func predastoreTopologyBlock(topo *predastoreTopology) []string {
	out := []string{
		"",
		"# One host per machine: a predastore process owning a socket and a data",
		"# directory. bind_addr binds every interface; public_addr is what the other",
		"# hosts dial.",
	}
	for _, h := range topo.Hosts {
		out = append(out,
			"",
			"[[host]]",
			fmt.Sprintf("id = %d", h.ID),
			fmt.Sprintf("bind_addr = %q", h.BindAddr),
			fmt.Sprintf("public_addr = %q", h.PublicAddr),
			fmt.Sprintf("data_dir = %q", h.DataDir),
		)
	}

	out = append(out,
		"",
		"# Nodes are roles pinned to a host: shard-storage nodes hold erasure-coded",
		"# object shards, state replicas form the Raft quorum over global state.",
	)
	for _, n := range topo.Nodes {
		out = append(out,
			"",
			"[[node]]",
			fmt.Sprintf("id = %d", n.ID),
			fmt.Sprintf("host_id = %d", n.HostID),
			fmt.Sprintf("role = %q", n.Role),
		)
	}
	return out
}

// predastoreV4Path resolves a v4 node path, which is relative to the
// predastore base path unless the operator made it absolute.
func predastoreV4Path(basePath, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(basePath, path)
}

// predastoreBasePath mirrors SPINIFEX_PREDASTORE_BASE_PATH in
// build/systemd/spinifex-predastore.service; v4 node paths resolve against it.
func predastoreBasePath(spxRoot string) string {
	return filepath.Join(spxRoot, "predastore")
}

// predastoreDataDir mirrors admin.PredastoreDataDir. Duplicated to keep the
// migrate package free of the admin dependency; if either moves, both must.
func predastoreDataDir(spxRoot string) string {
	return filepath.Join(spxRoot, "predastore", "cluster")
}
