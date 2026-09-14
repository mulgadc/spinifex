package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// jetStreamStoreDir is where the NATS service keeps its JetStream data under a
// node's data directory. The nats.conf template sets store_dir to
// <data_dir>/nats/, and nats-server appends "jetstream" itself.
func jetStreamStoreDir(dataDir string) string {
	return filepath.Join(dataDir, "nats", "jetstream")
}

// localStreams lists the streams a JetStream store holds on disk, as
// "<account>/<stream>". A missing store holds nothing.
func localStreams(storeDir string) ([]string, error) {
	accounts, err := os.ReadDir(storeDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read JetStream store %s: %w", storeDir, err)
	}
	var streams []string
	for _, acc := range accounts {
		if !acc.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(storeDir, acc.Name(), "streams"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read streams for account %s: %w", acc.Name(), err)
		}
		for _, e := range entries {
			if e.IsDir() {
				streams = append(streams, acc.Name()+"/"+e.Name())
			}
		}
	}
	sort.Strings(streams)
	return streams, nil
}

// procRoot is overridable in tests so the process scan can run against a fake
// /proc tree.
var procRoot = "/proc"

// natsRunningLocally reports whether this host runs the Spinifex NATS service,
// found as a process whose command line is `spx service nats ...`. Removing a
// store under a live server races JetStream, which rewrites its index files.
func natsRunningLocally() (bool, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return false, fmt.Errorf("scan processes: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || strings.TrimLeft(e.Name(), "0123456789") != "" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		if isSpxNATSService(bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0})) {
			return true, nil
		}
	}
	return false, nil
}

// isSpxNATSService matches `spx service nats` with any leading path on spx.
func isSpxNATSService(argv [][]byte) bool {
	return len(argv) >= 3 &&
		filepath.Base(string(argv[0])) == "spx" &&
		string(argv[1]) == "service" &&
		string(argv[2]) == "nats"
}

// errNATSRunning is returned whenever a store would be discarded with the NATS
// service still up. No flag overrides it: the wipe would race the live server.
var errNATSRunning = errors.New("the NATS service is still running on this node; stop it first (sudo systemctl stop spinifex.target, then confirm `pgrep -af 'spx service'` prints nothing)")

// checkStoreDiscardable fails if the NATS service is running locally.
func checkStoreDiscardable() error {
	running, err := natsRunningLocally()
	if err != nil {
		return err
	}
	if running {
		return errNATSRunning
	}
	return nil
}

// discardJetStreamStore removes a node's JetStream store before it enters a
// multi-node cluster. nats-server adopts a same-named local stream and raft
// catch-up never repairs the prefix it already holds.
func discardJetStreamStore(storeDir string) error {
	if err := checkStoreDiscardable(); err != nil {
		return err
	}
	if err := os.RemoveAll(storeDir); err != nil {
		return fmt.Errorf("remove JetStream store %s: %w", storeDir, err)
	}
	if _, err := os.Stat(storeDir); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("JetStream store %s still present after removal", storeDir)
	}
	return nil
}

// checkInitJetStreamStore refuses a multi-node init over a store holding streams
// when discard was turned off. Keeping it as a seed is not safe: leadership
// ignores who holds data, and unassigned local streams are deleted as orphans.
func checkInitJetStreamStore(storeDir string, nodes int, discard bool) error {
	if nodes < 2 {
		return nil
	}
	streams, err := localStreams(storeDir)
	if err != nil {
		return err
	}
	if len(streams) == 0 {
		return nil
	}
	if !discard {
		return fmt.Errorf("this node's JetStream store %s holds %d stream(s) from before formation; "+
			"a multi-node cluster cannot adopt them consistently. "+
			"re-run without --discard-jetstream=false to remove them once every node has joined", storeDir, len(streams))
	}
	return checkStoreDiscardable()
}
